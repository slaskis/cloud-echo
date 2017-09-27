package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	speech "cloud.google.com/go/speech/apiv1beta1"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/polly"
	speechpb "google.golang.org/genproto/googleapis/cloud/speech/v1beta1"
)

type options struct {
	sampleRate int
	language   string
	codec      string
}

var opts = options{}

func init() {
	flag.IntVar(&opts.sampleRate, "sample-rate", 16000, "sample rate of stream")
	flag.StringVar(&opts.language, "language", "sv-SE", "language to parse")
	flag.StringVar(&opts.codec, "codec", "flac", "audio codec")
	flag.Parse()
}

// build and run with:
//
//   sox -d  -r 16k -c 1 -t flac - | ./main
//
func main() {
	var wg sync.WaitGroup
	ctx := context.Background()
	svc := polly.New(session.New())

	resp, err := svc.DescribeVoices(&polly.DescribeVoicesInput{
		LanguageCode: aws.String(opts.language),
	})
	if err != nil {
		log.Fatalf("Failed to get voices: %v", err)
	}
	voice := *resp.Voices[0].Id

	// Creates a client.
	client, err := speech.NewClient(ctx)
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}

	stream, err := client.StreamingRecognize(ctx)
	if err != nil {
		log.Fatal(err)
	}

	codec, ok := speechpb.RecognitionConfig_AudioEncoding_value[strings.ToUpper(opts.codec)]
	if !ok {
		log.Fatalf("Invalid codec: %s", opts.codec)
	}

	// send the initial configuration message.
	err = stream.Send(&speechpb.StreamingRecognizeRequest{
		StreamingRequest: &speechpb.StreamingRecognizeRequest_StreamingConfig{
			StreamingConfig: &speechpb.StreamingRecognitionConfig{
				Config: &speechpb.RecognitionConfig{
					LanguageCode: opts.language,
					Encoding:     speechpb.RecognitionConfig_AudioEncoding(codec),
					SampleRate:   int32(opts.sampleRate),
				},
			},
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("sent config. now listening on stdin")

	texts := make(chan string)
	streams := make(chan io.ReadCloser)

	cmd := exec.CommandContext(ctx, "sh", "-c", "/usr/local/bin/sox -d -r "+strconv.Itoa(opts.sampleRate)+" -c 1 -t "+opts.codec+" -")
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatal(err)
	}
	defer out.Close()

	err = cmd.Start()
	if err != nil {
		log.Fatalf("start: %v", err)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		fmt.Print("Press 'Enter' to stop")
		bufio.NewReader(os.Stdin).ReadBytes('\n')
		err := cmd.Process.Signal(os.Interrupt)
		if err != nil {
			log.Fatal(err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()

		// pipe stdin to the API
		buf := make([]byte, 1024)
		for {
			n, err := out.Read(buf)
			if err == io.EOF {
				// Nothing else to pipe, close the stream.
				if err := stream.CloseSend(); err != nil {
					log.Fatalf("Could not close stream: %v", err)
				}
				log.Printf("sent all the audio")
				return
			}
			if err != nil {
				log.Printf("Could not read from stdin: %v", err)
				continue
			}
			err = stream.Send(&speechpb.StreamingRecognizeRequest{
				StreamingRequest: &speechpb.StreamingRecognizeRequest_AudioContent{
					AudioContent: buf[:n],
				},
			})
			if err != nil {
				log.Printf("Could not send audio: %v", err)
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				log.Printf("recv eof %v", resp)
				close(texts)
				break
			}
			if err != nil {
				log.Fatalf("Cannot stream results: %v", err)
			}
			if err := resp.Error; err != nil {
				log.Fatalf("Could not recognize: %v", err)
			}
			for _, result := range resp.Results {
				log.Printf("Result: %s", result)
				for _, alt := range result.Alternatives {
					texts <- alt.Transcript
				}
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		err := cmd.Wait()
		if err != nil {
			log.Fatalf("wait: %v", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for text := range texts {
			stream, err := say(svc, voice, text)
			if err != nil {
				break
			}
			streams <- stream
		}
		close(streams)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for stream := range streams {
			now := time.Now().Format(time.UnixDate)
			name := "./tmp/" + now + ".mp3"
			file, err := os.Create(name)
			if err != nil {
				log.Fatal(err)
				break
			}
			defer file.Close()
			_, err = io.Copy(file, stream)
			defer stream.Close()
			log.Printf("wrote audio to " + name)
		}
	}()

	wg.Wait()
}

func say(svc *polly.Polly, voice string, text string) (io.ReadCloser, error) {
	log.Printf("saying '%s'", text)
	input := &polly.SynthesizeSpeechInput{
		OutputFormat: aws.String("mp3"),
		SampleRate:   aws.String("8000"),
		Text:         aws.String(text),
		TextType:     aws.String("text"),
		VoiceId:      aws.String(voice),
	}

	result, err := svc.SynthesizeSpeech(input)
	if err != nil {
		return nil, err
	}
	return result.AudioStream, nil
}
