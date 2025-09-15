package main

import (
	"audiomodpoc"
	"fmt"

	"github.com/gordonklaus/portaudio"
	audio "github.com/oliviamiller/audioapi-poc"
	"go.viam.com/rdk/module"
	"go.viam.com/rdk/resource"
)

func main() {

	if err := portaudio.Initialize(); err != nil {
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
	sampleRate := 44100

	// Buffer to hold audio samples
	buffer := make([]float32, framesPerBuffer*channels)
	// Open input stream
	fmt.Printf("Opening stream: channels=%d, sampleRate=%d, framesPerBuffer=%d, bufferLen=%d\n",
		channels, sampleRate, framesPerBuffer, len(buffer))

	stream, err := portaudio.OpenDefaultStream(
		channels,            // input channels
		0,                   // output channels (0 for input only)
		float64(sampleRate), // sample rate
		framesPerBuffer,     // frames per buffer
		buffer,              // buffer
	)

	stream.Start()

// 	// Read audio data
// 	if err := stream.Read(); err != nil {
// 		fmt.Println("error reading")
// 	}

// 	for i:=0; i< 10; i++ {
// 	// Convert float32 samples to int16 PCM bytes
// 	pcmData := make([]byte, len(buffer)*2) // 2 bytes per int16 sample
// 	for i, sample := range buffer {
// 		fmt.Println(sample)
		
// 		// Clamp sample to valid range
// 		if sample > 1.0 {
// 			sample = 1.0
// 		} else if sample < -1.0 {
// 			sample = -1.0
// 		}
// 		// Convert float32 to int16
// 		intSample := int16(sample * 32767.0)
// 		// Write as little-endian bytes
// 		pcmData[i*2] = byte(intSample & 0xFF)
// 		pcmData[i*2+1] = byte((intSample >> 8) & 0xFF)
// 	}
// }


	stream.Close()
	portaudio.Terminate()

	// ModularMain can take multiple APIModel arguments, if your module implements multiple models.
	module.ModularMain(resource.APIModel{audio.API, audiomodpoc.Audioin})
}
