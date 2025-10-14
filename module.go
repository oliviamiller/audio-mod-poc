package audiomodpoc

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	mp32 "github.com/hajimehoshi/go-mp3"

	"github.com/braheezy/shine-mp3/pkg/mp3"

	"github.com/gordonklaus/portaudio"
	"go.viam.com/rdk/components/audioin"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"

	// Import audio drivers for macOS
	_ "github.com/pion/mediadevices/pkg/driver/audiotest"
	_ "github.com/pion/mediadevices/pkg/driver/microphone"
)

var (
	Audioin          = resource.NewModel("olivia", "audioin", "microphone")
	errUnimplemented = errors.New("unimplemented")
)

func init() {
	// Register the component using the API from your audioapi-poc
	resource.RegisterComponent(audioin.API, Audioin,
		resource.Registration[audioin.AudioIn, *Config]{
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

func newAudioModPocAudioin(ctx context.Context, deps resource.Dependencies, rawConf resource.Config, logger logging.Logger) (audioin.AudioIn, error) {
	conf, err := resource.NativeConfig[*Config](rawConf)
	if err != nil {
		return nil, err
	}

	return NewAudioin(ctx, deps, rawConf.ResourceName(), conf, logger)
}

func NewAudioin(ctx context.Context, deps resource.Dependencies, name resource.Name, conf *Config, logger logging.Logger) (audioin.AudioIn, error) {
	cancelCtx, cancelFunc := context.WithCancel(context.Background())

	audioCapturer := NewAudioCapturer("pcm32_float") // Default format

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

// const sampleRate = 45000

// AudioCapturer handles audio capture and streaming via channels
type AudioCapturer struct {
	stream     *portaudio.Stream
	buffer     interface{} // Can be []int16, []float32, or []int32
	format     string
	sequence   int32
	isRunning  bool
	mp3Encoder *mp3.Encoder // MP3 encoder for persistent encoding
}

// NewAudioCapturer creates a new audio capturer
func NewAudioCapturer(format string) *AudioCapturer {
	return &AudioCapturer{
		format: format,
	}
}

// StartCapture initializes audio capture and returns a channel of audio chunks
func (ac *AudioCapturer) StartCapture(ctx context.Context, codec string, sampleRate int, channels int) (chan *audioin.AudioChunk, error) {

	defaultInput, err := portaudio.DefaultInputDevice()
	if err != nil {
		fmt.Printf("Error getting default input device: %v\n", err)
	} else {
		fmt.Printf("Default input device: %s (inputs: %d)\n",
			defaultInput.Name, defaultInput.MaxInputChannels)
	}

	// Audio parameters
	const framesPerBuffer = 512

	// Buffer to hold audio samples based on format
	var bufferLen int
	switch codec {
	case "pcm16", "mp3":
		ac.buffer = make([]int16, framesPerBuffer*channels)
		bufferLen = len(ac.buffer.([]int16))
	case "pcm32":
		ac.buffer = make([]int32, framesPerBuffer*channels)
		bufferLen = len(ac.buffer.([]int32))
	case "pcm32_float":
		ac.buffer = make([]float32, framesPerBuffer*channels)
		bufferLen = len(ac.buffer.([]float32))
	default:
		ac.buffer = make([]float32, framesPerBuffer*channels) // Default to float32
		bufferLen = len(ac.buffer.([]float32))
	}

	dev, err := portaudio.DefaultInputDevice()
	if err != nil {
		return nil, fmt.Errorf("failed to get default input device: %w", err)
	}

	in := portaudio.StreamDeviceParameters{
		Device:   dev,
		Channels: channels,
		Latency:  10 * time.Millisecond,
	}

	params := portaudio.StreamParameters{
		Input:           in,
		SampleRate:      float64(sampleRate),
		FramesPerBuffer: framesPerBuffer,
	}

	// Initialize MP3 encoder if needed
	if codec == "mp3" {
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
	chunkChan := make(chan *audioin.AudioChunk, 10) // Buffer for smoother streaming

	// Start goroutine to capture audio
	go ac.captureLoop(ctx, chunkChan, codec, sampleRate, channels)

	return chunkChan, nil
}

// captureLoop runs the audio capture loop
func (ac *AudioCapturer) captureLoop(ctx context.Context, chunkChan chan *audioin.AudioChunk, codec string, sampleRate int, channels int) {
	defer func() {
		close(chunkChan)
		ac.Stop()
	}()

	for ac.isRunning {
		select {
		case <-ctx.Done():
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
			fmt.Printf("error reading stream: %w\ns", err)
			return

		}

		var pcmData []byte
		switch codec {
		case "pcm16":
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
		case "pcm32":
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
		case "pcm32_float":
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
		case "mp3":
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
		chunk := &audioin.AudioChunk{
			AudioData: pcmData,
			Sequence:  int32(ac.sequence),
		}

		ac.sequence++

		// Send chunk to channel
		select {
		case chunkChan <- chunk:
		case <-ctx.Done():
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

func (s *audioModPocAudioin) GetAudio(ctx context.Context, codec string, durationSeconds float32, previousTimeStamp int64, extra map[string]interface{}) (chan *audioin.AudioChunk, error) {
	s.logger.Infof("Starting audio recording for %d seconds", durationSeconds)

	var captureCtx context.Context
	captureCtx = ctx
	if durationSeconds != 0 {
		captureCtx, _ = context.WithTimeout(ctx, time.Duration(durationSeconds)*time.Second)
	}
	// hardcorded for now. need to add these params to the config
	audio, err := s.audioCapturer.StartCapture(captureCtx, "pcm16", 48000, 1)
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

func (s *audioModPocAudioin) Play(ctx context.Context, audio []byte, codec string, sampleRate int, channels int) error {
	s.logger.Infof("Playing the audio")

	defaultOutput, err := portaudio.DefaultOutputDevice()
	if err != nil {
		fmt.Printf("Error getting default output device: %v\n", err)
	} else {
		fmt.Printf("Default output device: %s (outputs: %d), sample rate %d \n",
			defaultOutput.Name, defaultOutput.MaxOutputChannels, defaultOutput.DefaultSampleRate)
	}

	audioSamples := make([]int16, len(audio)/2)
	framesPerBuffer := 512

	switch codec {
	case "pcm16":
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
	case "mp3":
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
			2,               // output channels
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

		// Create intermediate buffer for MP3 decoder's natural chunk size
		audioBuffer := make([]int16, 0)
		samplesPerBuffer := framesPerBuffer * chans

		eofReached := false
		totalSamplesPlayed := 0

		for {
			fmt.Printf("DEBUG: Loop iteration, audioBuffer len=%d,eofReached=%v\n", len(audioBuffer), eofReached)
			// Read more MP3 data if we need it and haven't reached EOF
			for len(audioBuffer) < samplesPerBuffer && !eofReached {
				// Let MP3 decoder read its natural chunk size
				tempBuffer := make([]byte, 4608)
				numRead, err := decoder.Read(tempBuffer)

				fmt.Printf("MP3 Read: numRead=%d, err=%v, audioBuffer len=%d, eofReached=%v\n",
					numRead, err, len(audioBuffer), eofReached)

				if err != nil && err != io.EOF {
					return fmt.Errorf("error reading: %w", err)
				}

				if numRead == 0 {
					// End of file reached
					fmt.Printf("EOF reached, audioBuffer has %d samples remaining\n", len(audioBuffer))
					eofReached = true
					break
				}

				// Convert bytes to int16 samples and append to buffer
				newSamples := make([]int16, numRead/2)
				err = binary.Read(bytes.NewReader(tempBuffer[:numRead]), binary.LittleEndian, newSamples)
				if err != nil {
					return fmt.Errorf("error converting to int16: %w", err)
				}
				audioBuffer = append(audioBuffer, newSamples...)
				fmt.Printf("Added %d samples to buffer, total buffer: %d samples\n", len(newSamples), len(audioBuffer))
			}

			// If we have no samples left, we're done
			if len(audioBuffer) == 0 {
				fmt.Printf("No more samples in buffer, ending playback. Total played: %d samples\n", totalSamplesPlayed)
				break
			}

			// Extract samples for PortAudio - play whatever we have
			samplesToPlay := len(audioBuffer)
			if samplesToPlay > samplesPerBuffer {
				samplesToPlay = samplesPerBuffer
			}

			fmt.Printf("Playing %d samples (buffer has %d, need %d)\n", samplesToPlay, len(audioBuffer), samplesPerBuffer)

			// Copy samples to output buffer
			copy(outputBuffer[:samplesToPlay], audioBuffer[:samplesToPlay])

			// Zero-pad if needed
			zeroPadded := 0
			for i := samplesToPlay; i < samplesPerBuffer; i++ {
				outputBuffer[i] = 0
				zeroPadded++
			}
			if zeroPadded > 0 {
				fmt.Printf("Zero-padded %d samples\n", zeroPadded)
			}

			// Remove used samples from buffer
			audioBuffer = audioBuffer[samplesToPlay:]
			totalSamplesPlayed += samplesToPlay

			fmt.Printf("DEBUG: About to write %d samples to stream\n",
				samplesToPlay)

			// Use a channel to implement timeout for stream.Write()
			done := make(chan error, 1)
			go func() {
				done <- stream.Write()
			}()

			select {
			case err := <-done:
				if err != nil {
					return fmt.Errorf("error writing to stream: %w", err)
				}
			case <-time.After(1 * time.Second):
				return fmt.Errorf("stream write timed out - audio system conflict")
			}

			fmt.Printf("wrote to stream")
		}

		fmt.Printf("MP3 playback complete. Total samples played: %d\n", totalSamplesPlayed)

		if err := stream.Stop(); err != nil {
			return fmt.Errorf("failed to stop stream: %w", err)
		}
	default:
		return errors.New("format not supported yet")
	}

	fmt.Println("audio successfully played!")

	return nil
}

func (s *audioModPocAudioin) Properties(ctx context.Context, extra map[string]interface{}) (audioin.Properties, error) {
	return audioin.Properties{}, nil
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
