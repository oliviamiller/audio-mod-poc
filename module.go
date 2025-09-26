package audiomodpoc

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/braheezy/shine-mp3/pkg/mp3"
	mp32 "github.com/hajimehoshi/go-mp3"

	audioapi "github.com/oliviamiller/audioapi-poc"
	pb "github.com/oliviamiller/audioapi-poc/grpc"

	"github.com/gordonklaus/portaudio"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"

	// Import audio drivers for macOS
	_ "github.com/pion/mediadevices/pkg/driver/audiotest"
	_ "github.com/pion/mediadevices/pkg/driver/microphone"
)

var (
	Audioin          = resource.NewModel("olivia", "audio", "audioin")
	errUnimplemented = errors.New("unimplemented")
)

func init() {
	// Register the component using the API from your audioapi-poc
	resource.RegisterComponent(audioapi.API, Audioin,
		resource.Registration[audioapi.Audio, *Config]{
			Constructor: newAudioModPocAudioin,
		},
	)

}

type Config struct {
}

// Validate ensures all parts of the config are valid and important fields exist.
// Returns implicit required (first return) and optional (second return) dependencies based on the config.
// The path is the JSON path in your robot's config (not the `Config` struct) to the
// resource being validated; e.g. "components.0".
func (cfg *Config) Validate(path string) ([]string, []string, error) {
	// Add config validation code here
	return nil, nil, nil
}

type audioModPocAudioin struct {
	resource.AlwaysRebuild

	name resource.Name

	logger logging.Logger
	cfg    *Config

	audioCapturer *AudioCapturer
	cancelCtx     context.Context
	cancelFunc    func()
}

func newAudioModPocAudioin(ctx context.Context, deps resource.Dependencies, rawConf resource.Config, logger logging.Logger) (audioapi.Audio, error) {
	conf, err := resource.NativeConfig[*Config](rawConf)
	if err != nil {
		return nil, err
	}

	return NewAudioin(ctx, deps, rawConf.ResourceName(), conf, logger)
}

func NewAudioin(ctx context.Context, deps resource.Dependencies, name resource.Name, conf *Config, logger logging.Logger) (audioapi.Audio, error) {
	cancelCtx, cancelFunc := context.WithCancel(context.Background())

	audioCapturer := NewAudioCapturer(audioapi.Pcm32Float) // Default format

	// Initialize PortAudio
	if err := portaudio.Initialize(); err != nil {
		return nil, fmt.Errorf("failed to initialize PortAudio: %w", err)
	}

	// Debug: List available devices
	fmt.Println("=== Available Audio Devices ===")
	devices, err := portaudio.Devices()
	if err != nil {
		fmt.Printf("Error getting devices: %v\n", err)
	} else {
		for i, device := range devices {
			fmt.Printf("Device %d: %s (inputs: %d, outputs: %d)\n",
				i, device.Name, device.MaxInputChannels, device.MaxOutputChannels)
		}
	}

	s := &audioModPocAudioin{
		name:          name,
		logger:        logger,
		cfg:           conf,
		audioCapturer: audioCapturer,
		cancelCtx:     cancelCtx,
		cancelFunc:    cancelFunc,
	}
	return s, nil
}

func (s *audioModPocAudioin) Name() resource.Name {
	return s.name
}

//const sampleRate = 44100

// AudioCapturer handles audio capture and streaming via channels
type AudioCapturer struct {
	stream     *portaudio.Stream
	buffer     interface{} // Can be []int16, []float32, or []int32
	format     audioapi.AudioFormat
	sequence   int32
	isRunning  bool
	mp3Encoder *mp3.Encoder // MP3 encoder for persistent encoding
}

// NewAudioCapturer creates a new audio capturer
func NewAudioCapturer(format audioapi.AudioFormat) *AudioCapturer {
	return &AudioCapturer{
		format: format,
	}
}

// StartCapture initializes audio capture and returns a channel of audio chunks
func (ac *AudioCapturer) StartCapture(ctx context.Context, format audioapi.AudioFormat, sampleRate int, channels int) (<-chan *audioapi.AudioChunk, error) {

	startTime := time.Now().UnixMilli()
	defaultInput, err := portaudio.DefaultInputDevice()
	if err != nil {
		fmt.Printf("Error getting default input device: %v\n", err)
	} else {
		fmt.Printf("Default input device: %s (inputs: %d)\n",
			defaultInput.Name, defaultInput.MaxInputChannels)
	}

	// Audio parameters
	const framesPerBuffer = 2048

	// Buffer to hold audio samples based on format
	switch format {
	case audioapi.Pcm16, audioapi.Mp3:
		ac.buffer = make([]int16, framesPerBuffer*channels)
	case audioapi.Pcm32:
		ac.buffer = make([]int32, framesPerBuffer*channels)
	case audioapi.Pcm32Float:
		ac.buffer = make([]float32, framesPerBuffer*channels)
	default:
		ac.buffer = make([]float32, framesPerBuffer*channels) // Default to float32
	}

	dev, err := portaudio.DefaultInputDevice()
	if err != nil {
		return nil, fmt.Errorf("failed to get default input device: %w", err)
	}

	in := portaudio.StreamDeviceParameters{
		Device:   dev,
		Channels: channels,
		Latency:  100 * time.Millisecond,
	}

	params := portaudio.StreamParameters{
		Input:           in,
		SampleRate:      float64(sampleRate),
		FramesPerBuffer: framesPerBuffer,
	}

	// Get buffer length based on type
	var bufferLen int
	switch format {
	case audioapi.Pcm16:
		bufferLen = len(ac.buffer.([]int16))
	case audioapi.Pcm32:
		bufferLen = len(ac.buffer.([]int32))
	case audioapi.Pcm32Float:
		bufferLen = len(ac.buffer.([]float32))
	case audioapi.Mp3:
		bufferLen = len(ac.buffer.([]int16))
	default:
		bufferLen = len(ac.buffer.([]float32))
	}

	// Initialize MP3 encoder if needed
	if format == audioapi.Mp3 {
		ac.mp3Encoder = mp3.NewEncoder(sampleRate, channels)
		fmt.Printf("Initialized MP3 encoder for %d Hz, %d channels\n", sampleRate, channels)
	}

	// Open input stream
	fmt.Printf("Opening stream: channels=%d, sampleRate=%d, framesPerBuffer=%d, bufferLen=%d\n",
		channels, sampleRate, framesPerBuffer, bufferLen)

	ac.stream, err = portaudio.OpenStream(params, ac.buffer)
	if err != nil {
		fmt.Println("HERE FAILED TO OPEN STREAM")
		fmt.Println(err)
		return nil, fmt.Errorf("failed to open stream: %w", err)
	}

	fmt.Printf("Stream opened successfully. Info: %+v\n", ac.stream.Info())

	if err := ac.stream.Start(); err != nil {
		ac.stream.Close()
		portaudio.Terminate()
		return nil, fmt.Errorf("failed to start stream: %w", err)
	}

	ac.sequence = 0
	ac.isRunning = true

	// Create channels for audio chunks and errors
	chunkChan := make(chan *audioapi.AudioChunk, 10) // Buffer for smoother streaming

	endTime := time.Now().UnixMilli()

	fmt.Println("TIME")
	fmt.Println(endTime - startTime)

	// Start goroutine to capture audio
	go ac.captureLoop(ctx, chunkChan, format, sampleRate, channels)

	return chunkChan, nil
}

// captureLoop runs the audio capture loop
func (ac *AudioCapturer) captureLoop(ctx context.Context, chunkChan chan<- *audioapi.AudioChunk, format audioapi.AudioFormat, sampleRate int, channels int) {
	defer func() {
		close(chunkChan)
		ac.Stop()
	}()

	for ac.isRunning {
		select {
		case <-ctx.Done():
			fmt.Println("context done, returning")
			return
		default:
		}

		// Read audio data
		if err := ac.stream.Read(); err != nil {
			// Handle input overflow gracefully - common on Linux
			if err.Error() == "Input overflowed" {
				fmt.Println("Audio input overflow, skipping buffer...")
				continue // Skip this buffer and continue capturing
			}

			// Create audio chunk
			chunk := &audioapi.AudioChunk{
				Err: fmt.Errorf("failed to read stream: %w", err),
			}
			// Send chunk to channel
			select {
			case chunkChan <- chunk:
			case <-ctx.Done():
				fmt.Println("context is done")
				return
			}
			return
		}

		var pcmData []byte
		switch format {
		case audioapi.Pcm16:
			buffer := ac.buffer.([]int16)
			buf := new(bytes.Buffer)

			// Write int16 audio data to byte array in little-endian
			for _, val := range buffer {
				err := binary.Write(buf, binary.LittleEndian, val)
				if err != nil {
					fmt.Println("Error writing int16:", err)
					return
				}
			}
			pcmData = buf.Bytes()
		case audioapi.Pcm32:
			buffer := ac.buffer.([]int32)
			buf := new(bytes.Buffer)

			// Write int32 audio data to byte array in little-endian
			for _, val := range buffer {
				err := binary.Write(buf, binary.LittleEndian, val)
				if err != nil {
					fmt.Println("Error writing int32:", err)
					return
				}
			}
			pcmData = buf.Bytes()
		case audioapi.Pcm32Float:
			buffer := ac.buffer.([]float32)
			buf := new(bytes.Buffer)

			// Write float32 audio data to byte array in little-endian
			for _, val := range buffer {
				err := binary.Write(buf, binary.LittleEndian, val)
				if err != nil {
					fmt.Println("Error writing float32:", err)
					return
				}
			}
			pcmData = buf.Bytes()
		case audioapi.Mp3:
			// Create buffer for MP3 output
			var mp3Buffer bytes.Buffer

			// Encode to MP3 using persistent encoder
			buffer := ac.buffer.([]int16)
			err := ac.mp3Encoder.Write(&mp3Buffer, buffer)
			if err != nil {
				fmt.Println("error writing mp3Encoder", err)
				return
			}
			fmt.Printf("MP3 buffer size: %d bytes\n", mp3Buffer.Len())
			pcmData = mp3Buffer.Bytes()

		default:
			// Fallback to float32 converted to int16
			buffer := ac.buffer.([]float32)

			pcmData = make([]byte, len(buffer)*2) // 2 bytes per int16 sample
			for i, sample := range buffer {
				// Clamp sample to valid range
				if sample > 1.0 {
					sample = 1.0
				} else if sample < -1.0 {
					sample = -1.0
				}
				// Convert float32 to int16
				intSample := int16(sample * 32767.0)
				// Write as little-endian bytes
				pcmData[i*2] = byte(intSample & 0xFF)
				pcmData[i*2+1] = byte((intSample >> 8) & 0xFF)
			}
		}

		// Create audio chunk
		chunk := &audioapi.AudioChunk{
			AudioData: pcmData,
			Sequence:  int64(ac.sequence),
		}

		ac.sequence++

		// Send chunk to channel
		select {
		case chunkChan <- chunk:
		case <-ctx.Done():
			fmt.Println("context is done")
			return
		}
	}
}

// Stop stops the audio capture
func (ac *AudioCapturer) Stop() {
	ac.isRunning = false
	if ac.stream != nil {
		ac.stream.Stop()
		ac.stream.Close()
		ac.stream = nil
	}

	// Clean up MP3 encoder if it exists
	if ac.mp3Encoder != nil {
		ac.mp3Encoder = nil
	}

}

func (s *audioModPocAudioin) Record(ctx context.Context, info audioapi.AudioInfo, durationSeconds int) (<-chan *audioapi.AudioChunk, error) {
	s.logger.Infof("Starting audio recording for %d seconds", durationSeconds)

	var captureCtx context.Context
	captureCtx = ctx
	if durationSeconds != 0 {
		captureCtx, _ = context.WithTimeout(ctx, time.Duration(durationSeconds)*time.Second)
	}
	audio, err := s.audioCapturer.StartCapture(captureCtx, info.Format, info.SampleRate, info.Channels)
	if err != nil {
		return nil, err
	}

	fmt.Println("module is capturing audio.....")

	// todo: figure out cleanup up
	// // Return cleanup functiona
	// cleanup := func() {
	// 	capturer.Stop()
	// }

	return audio, nil

}

func (s *audioModPocAudioin) Play(ctx context.Context, audio []byte, format pb.AudioFormat, sampleRate int, channels int) error {
	s.logger.Infof("Playing the audio")

	// Initialize PortAudio
	if err := portaudio.Initialize(); err != nil {
		return fmt.Errorf("failed to initialize PortAudio: %w", err)
	}
	defer portaudio.Terminate()

	defaultOutput, err := portaudio.DefaultOutputDevice()
	if err != nil {
		fmt.Printf("Error getting default output device: %v\n", err)
	} else {
		fmt.Printf("Default output device: %s (outputs: %d), sample rate %d \n",
			defaultOutput.Name, defaultOutput.MaxOutputChannels, defaultOutput.DefaultSampleRate)
	}

	audioSamples := make([]int16, len(audio)/2)
	framesPerBuffer := 2048

	switch format {
	case pb.AudioFormat_PCM16:
		err := binary.Read(bytes.NewReader(audio), binary.LittleEndian, &audioSamples)
		if err != nil {
			return fmt.Errorf("could not convert to int16 array: %w", err)
		}
		outputBuffer := make([]int16, framesPerBuffer*channels)

		// dev, err := portaudio.DefaultOutputDevice()
		// if err != nil {
		// 	return err
		// }

		// out := portaudio.StreamDeviceParameters{
		// 	Device:   dev,
		// 	Channels: channels,
		// 	Latency:  100 * time.Millisecond,
		// }

		// Open output stream
		stream, err := portaudio.OpenDefaultStream(
			0,                   // input channels
			channels,            // output channels
			float64(sampleRate), // sample rate
			framesPerBuffer,     // frames per buffer
			outputBuffer,        // buffer
		)

		if err != nil {
			return fmt.Errorf("failed to open stream: %w", err)
		}
		defer stream.Close()

		info := stream.Info()
		fmt.Printf("Input latency:  %d sec\n", info.InputLatency.Milliseconds())
		fmt.Printf("Output latency: %d sec\n", info.OutputLatency.Milliseconds())
		fmt.Printf("Sample rate:    %f\n", info.SampleRate)

		if err := stream.Start(); err != nil {
			return fmt.Errorf("failed to start stream: %w", err)
		}

		// Play audio in chunks
		totalSamples := len(audioSamples)
		samplesPerBuffer := framesPerBuffer * channels

		for offset := 0; offset < totalSamples; offset += samplesPerBuffer {
			// Copy samples to buffer
			end := offset + samplesPerBuffer
			if end > totalSamples {
				end = totalSamples
			}

			n := copy(outputBuffer, audioSamples[offset:end])
			if n < samplesPerBuffer {
				// pad with zeros
				for i := n; i < samplesPerBuffer; i++ {
					outputBuffer[i] = 0
				}
			}

			// Write buffer to stream
			if err := stream.Write(); err != nil {
				return fmt.Errorf("failed to write stream: %w", err)
			}
		}

		if err := stream.Stop(); err != nil {
			return fmt.Errorf("failed to stop stream: %w", err)
		}
	case pb.AudioFormat_MP3:
		reader := bytes.NewReader(audio)
		decoder, err := mp32.NewDecoder(reader)
		if err != nil {
			return fmt.Errorf("failed to get decoder: %w", err)
		}
		rate := decoder.SampleRate()
		chans := 2
		outputBuffer := make([]int16, framesPerBuffer*chans)

		fmt.Printf("MP3 sample rate: %d, channels: %d\n", rate, chans)

		stream, err := portaudio.OpenDefaultStream(
			0,               // input channels
			chans,           // output channels
			float64(rate),   // sample rate
			framesPerBuffer, // frames per buffer
			outputBuffer,    // buffer
		)

		if err != nil {
			return fmt.Errorf("failed to open stream: %w", err)
		}
		defer stream.Close()

		if err := stream.Start(); err != nil {
			return fmt.Errorf("failed to start stream: %w", err)
		}

		for {
			// Read directly into byte buffer sized for outputBuffer
			pcmBytes := make([]byte, len(outputBuffer)*2) // 2 bytes per int16
			numRead, err := decoder.Read(pcmBytes)
			if err != nil && err != io.EOF {
				return fmt.Errorf("error reading: %w", err)
			}

			fmt.Println(numRead)

			if numRead == 0 {
				break
			}

			// Convert directly to outputBuffer
			samplesRead := numRead / 2 // 2 bytes per int16 sample
			err = binary.Read(bytes.NewReader(pcmBytes[:numRead]), binary.LittleEndian, outputBuffer[:samplesRead])
			if err != nil {
				return fmt.Errorf("error converting to int16: %w", err)
			}

			// Clear remaining buffer if partial read
			for i := samplesRead; i < len(outputBuffer); i++ {
				outputBuffer[i] = 0
			}

			if err := stream.Write(); err != nil {
				return fmt.Errorf("error writing to stream: %w", err)
			}
		}

		if err := stream.Stop(); err != nil {
			return fmt.Errorf("failed to stop stream: %w", err)
		}
	default:
		return errors.New("format not supported yet")
	}

	fmt.Println("audio successfully played!")

	return nil
}

func (s *audioModPocAudioin) Properties(ctx context.Context) (audioapi.Properties, error) {
	return audioapi.Properties{}, nil
}

func (s *audioModPocAudioin) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	return nil, errUnimplemented
}

func (s *audioModPocAudioin) Close(context.Context) error {
	if s.audioCapturer != nil {
		s.audioCapturer.Stop()
	}
	s.cancelFunc()
	return nil
}
