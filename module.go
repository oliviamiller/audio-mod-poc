package audiomodpoc

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"

	audioapi "github.com/oliviamiller/audioapi-poc"
	pb "github.com/oliviamiller/audioapi-poc/api/api/audio"

	"github.com/gordonklaus/portaudio"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"

	// Import audio drivers for macOS
	_ "github.com/pion/mediadevices/pkg/driver/audiotest"
	_ "github.com/pion/mediadevices/pkg/driver/microphone"
)

var (
	Audioin          = resource.NewModel("olivia-org", "audio", "audioin")
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

	audioCapturer := NewAudioCapturer()

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

const sampleRate = 44100

// AudioCapturer handles audio capture and streaming via channels
type AudioCapturer struct {
	stream    *portaudio.Stream
	buffer    []float32
	sequence  int32
	isRunning bool
}

// NewAudioCapturer creates a new audio capturer
func NewAudioCapturer() *AudioCapturer {
	return &AudioCapturer{}
}

// StartCapture initializes audio capture and returns a channel of audio chunks
func (ac *AudioCapturer) StartCapture(ctx context.Context) (<-chan *audioapi.AudioChunk, <-chan error, error) {
	// Initialize PortAudio
	if err := portaudio.Initialize(); err != nil {
		return nil, nil, fmt.Errorf("failed to initialize PortAudio: %w", err)
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

	defaultInput, err := portaudio.DefaultInputDevice()
	if err != nil {
		fmt.Printf("Error getting default input device: %v\n", err)
	} else {
		fmt.Printf("Default input device: %s (inputs: %d)\n",
			defaultInput.Name, defaultInput.MaxInputChannels)
	}

	// Audio parameters
	const framesPerBuffer = 1024
	const channels = 1 // Mono recording

	// Buffer to hold audio samples
	ac.buffer = make([]float32, framesPerBuffer*channels)

	// Try a quick test stream to trigger permission dialog
	fmt.Println("Testing microphone access to trigger permissions...")
	testBuffer := make([]float32, 64)
	testStream, err := portaudio.OpenDefaultStream(1, 0, float64(sampleRate), 64, testBuffer)
	if err == nil {
		testStream.Start()
		testStream.Read() // This should trigger the permission dialog
		testStream.Stop()
		testStream.Close()
		fmt.Println("Test stream completed")
	} else {
		fmt.Printf("Test stream failed: %v\n", err)
	}

	// Open input stream
	fmt.Printf("Opening stream: channels=%d, sampleRate=%d, framesPerBuffer=%d, bufferLen=%d\n",
		channels, sampleRate, framesPerBuffer, len(ac.buffer))

	ac.stream, err = portaudio.OpenDefaultStream(
		channels,            // input channels
		0,                   // output channels (0 for input only)
		float64(sampleRate), // sample rate
		framesPerBuffer,     // frames per buffer
		ac.buffer,           // buffer
	)
	if err != nil {
		portaudio.Terminate()
		return nil, nil, fmt.Errorf("failed to open stream: %w", err)
	}

	fmt.Printf("Stream opened successfully. Info: %+v\n", ac.stream.Info())

	if err := ac.stream.Start(); err != nil {
		ac.stream.Close()
		portaudio.Terminate()
		return nil, nil, fmt.Errorf("failed to start stream: %w", err)
	}

	ac.sequence = 0
	ac.isRunning = true

	// Create channels for audio chunks and errors
	chunkChan := make(chan *audioapi.AudioChunk, 10) // Buffer for smoother streaming
	errorChan := make(chan error, 1)

	// Start goroutine to capture audio
	go ac.captureLoop(ctx, chunkChan, errorChan)

	return chunkChan, errorChan, nil
}

// captureLoop runs the audio capture loop
func (ac *AudioCapturer) captureLoop(ctx context.Context, chunkChan chan<- *audioapi.AudioChunk, errorChan chan<- error) {
	defer func() {
		close(chunkChan)
		close(errorChan)
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
			if err.Error() == "input overflowed" {
				fmt.Println("Audio input overflow, skipping buffer...")
				continue // Skip this buffer and continue capturing
			}
			// For other errors, exit the loop
			select {
			case errorChan <- fmt.Errorf("failed to read stream: %w", err):
			case <-ctx.Done():
			}
			return
		}

		// Convert float32 samples to int16 PCM bytes
		pcmData := make([]byte, len(ac.buffer)*2) // 2 bytes per int16 sample
		for i, sample := range ac.buffer {
			if sample != 0.0 {
				fmt.Println(sample)
			}
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

		// Create audio chunk
		chunk := &audioapi.AudioChunk{
			AudioData: pcmData,
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
	portaudio.Terminate()
}

func captureAudio(filename string, durationSeconds int) error {
	fmt.Println("trying to capture audio")

	// Initialize PortAudio
	if err := portaudio.Initialize(); err != nil {
		return fmt.Errorf("failed to initialize PortAudio: %w", err)
	}
	defer portaudio.Terminate()

	// Audio parameters
	const framesPerBuffer = 1024
	const channels = 1 // Mono recording

	// Buffer to hold audio samples
	buffer := make([]float32, framesPerBuffer*channels)
	var recordedSamples []float32

	// Open input stream
	stream, err := portaudio.OpenDefaultStream(
		channels,            // input channels
		0,                   // output channels (0 for input only)
		float64(sampleRate), // sample rate
		framesPerBuffer,     // frames per buffer
		buffer,              // buffer
	)
	if err != nil {
		return fmt.Errorf("failed to open stream: %w", err)
	}
	defer stream.Close()

	if err := stream.Start(); err != nil {
		return fmt.Errorf("failed to start stream: %w", err)
	}
	fmt.Println("recording audio...")

	// Calculate number of reads needed
	numReads := (sampleRate * durationSeconds) / framesPerBuffer
	for i := 0; i < numReads; i++ {
		if err := stream.Read(); err != nil {
			return fmt.Errorf("failed to read stream: %w", err)
		}
		recordedSamples = append(recordedSamples, buffer...)
	}

	fmt.Println("stopping stream...")
	if err := stream.Stop(); err != nil {
		return fmt.Errorf("failed to stop stream: %w", err)
	}

	// Create output file
	file, err := os.Create(filename)
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}
	defer file.Close()

	// Write raw audio data as 16-bit PCM
	for _, sample := range recordedSamples {
		if sample != 0.0 {
		}
		// Clamp sample to valid range
		if sample > 1.0 {
			sample = 1.0
		} else if sample < -1.0 {
			sample = -1.0
		}
		// Convert float32 to int16
		intSample := int16(sample * 32767.0)
		if err := binary.Write(file, binary.LittleEndian, intSample); err != nil {
			return fmt.Errorf("failed to write sample: %w", err)
		}
	}

	fmt.Printf("Recorded %d samples to %s\n", len(recordedSamples), filename)
	return nil
}

func (s *audioModPocAudioin) Record(ctx context.Context, durationSeconds int) (<-chan *audioapi.AudioChunk, error) {
	s.logger.Infof("Starting audio recording for %d seconds", durationSeconds)

	audio, _, err := s.audioCapturer.StartCapture(ctx)
	if err != nil {
		return nil, err
	}

	fmt.Println("module is capturing audio.....")

	// todo: figure out cleanup up
	// // Return cleanup function
	// cleanup := func() {
	// 	capturer.Stop()
	// }

	return audio, nil

}

func (s *audioModPocAudioin) Play(ctx context.Context, audio []byte, format pb.FileFormat, sampleRate int, channels int) error {
	s.logger.Infof("Playing the audio")

	// Initialize PortAudio
	if err := portaudio.Initialize(); err != nil {
		return fmt.Errorf("failed to initialize PortAudio: %w", err)
	}
	defer portaudio.Terminate()

	switch format {
	case pb.FileFormat_PCM16:
		buffer := make([]int16, len(audio)/2)
		err := binary.Read(bytes.NewReader(audio), binary.LittleEndian, &buffer)
		if err != nil {
			return fmt.Errorf("could convert to int16 array: %w", err)
		}

		framesPerBuffer := 1024

		// Open input stream
		stream, err := portaudio.OpenDefaultStream(
			0,                   // input channels
			channels,            // output channels (0 for input only)
			float64(sampleRate), // sample rate
			framesPerBuffer,     // frames per buffer
			buffer,              // buffer
		)

		if err != nil {
			return fmt.Errorf("failed to open stream: %w", err)
		}
		defer stream.Close()

		if err := stream.Start(); err != nil {
			return fmt.Errorf("failed to start stream: %w", err)
		}

		// This will write whats in the buffer placed in openDefaultStream
		if err := stream.Write(); err != nil {
			return fmt.Errorf("failed to write stream: %w", err)
		}

	default:
		return errors.New("format not supported yet")
	}

	return nil
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
