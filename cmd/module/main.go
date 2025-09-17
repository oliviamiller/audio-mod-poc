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
	const framesPerBuffer = 256
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
	stream.Close()
	portaudio.Terminate()

	// ModularMain can take multiple APIModel arguments, if your module implements multiple models.
	module.ModularMain(resource.APIModel{audio.API, audiomodpoc.Audioin})
}
