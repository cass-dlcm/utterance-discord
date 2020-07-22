package main

import (
	speech "cloud.google.com/go/speech/apiv1"
	"context"
	"fmt"
	"github.com/bwmarrin/discordgo"
	speechpb "google.golang.org/genproto/googleapis/cloud/speech/v1"
	"io"
	"io/ioutil"
	"time"
	"log"
	"os"
	"os/signal"
	"syscall"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/pion/webrtc/v3/pkg/media/oggwriter"
)

func check(e error) {
	if e != nil {
		panic(e)
	}
}

func main() {
	dat, err := ioutil.ReadFile("token")
	check(err)

	dg, err := discordgo.New(string(dat))
	if err != nil {
		fmt.Println("error creating Discord session,", err)
		return
	}

	// Register messageCreate as a callback for the messageCreate events.
	dg.AddHandler(messageCreate)

	// We need information about guilds (which includes their channels),
	// messages and voice states.
	dg.Identify.Intents = discordgo.MakeIntent(discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentsGuildVoiceStates)

	err = dg.Open()
	if err != nil {
		fmt.Println("error opening connection,", err)
		return
	}
	fmt.Println("Bot is now running.  Press CTRL-C to exit.")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt, os.Kill)
	<-sc

	// Cleanly close down the Discord session.
	dg.Close()
}

func messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {

	// Ignore all messages created by the bot itself
	// This isn't required in this specific example but it's a good practice.
	if m.Author.ID == s.State.User.ID {
		return
	}
	// If the message is "ping" reply with "Pong!"
	if m.Content == "ping" {
		s.ChannelMessageSend(m.ChannelID, "Pong!")
	}

	if m.Content == "$transcribe" {
		// Find the channel that the message came from.
		c, err := s.State.Channel(m.ChannelID)
		if err != nil {
			// Could not find channel.
			return
		}

		// Find the guild for that channel.
		g, err := s.State.Guild(c.GuildID)
		if err != nil {
			// Could not find guild.
			return
		}

		// Look for the message sender in that guild's current voice states.
		for _, vs := range g.VoiceStates {
			if vs.UserID == m.Author.ID {
				transcribe(s, g.ID, vs.ChannelID)
				return
			}
		}
	}

}

func transcribe(s *discordgo.Session, guildID, channelID string) (err error) {

	// Join the provided voice channel.
	vc, err := s.ChannelVoiceJoin(guildID, channelID, false, false)
	if err != nil {
		return err
	}

	ctx := context.Background()

	client, err := speech.NewClient(ctx)
	if err != nil {
		log.Fatal(err)
	}

	go func() {
		time.Sleep(10 * time.Second)
		close(vc.OpusRecv)
		vc.Close()
	}()

	audioFiles := handleVoice(vc.OpusRecv)

	print(len(audioFiles))

	f := []*os.File{}
	for i, file := range audioFiles {
		fileVar, err := os.Open(file)
		if err != nil {
			log.Printf("%v\n", err)
		}
		f = append(f, fileVar)
		stream, err := client.StreamingRecognize(ctx)
		if err != nil {
			log.Fatal(err)
		}
		// Send the initial configuration message.
		if err := stream.Send(&speechpb.StreamingRecognizeRequest{
			StreamingRequest: &speechpb.StreamingRecognizeRequest_StreamingConfig{
				StreamingConfig: &speechpb.StreamingRecognitionConfig{
					Config:			// Workaround while the API doesn't give a more informative error.
	 					&speechpb.RecognitionConfig{
							Encoding:        speechpb.RecognitionConfig_OGG_OPUS,
							SampleRateHertz: 48000,
							LanguageCode:    "en-US",
							AudioChannelCount: 2,
					},
				},
			},
		}); err != nil {
			log.Fatal(err)
		}
		go func() {
			buf := make([]byte, 1024)
	    for {
				if i < len(f) {
		      n, err := f[i].Read(buf)
		      if n > 0 {
		        if err := stream.Send(&speechpb.StreamingRecognizeRequest{
		          StreamingRequest: &speechpb.StreamingRecognizeRequest_AudioContent{
		            AudioContent: buf[:n],
						  },
		  			}); err != nil {
		    			log.Printf("Could not send audio: %v", err)
		    		}
		    	}
		      if err == io.EOF {
		        // Nothing else to pipe, close the stream.
		        if err := stream.CloseSend(); err != nil {
		          log.Fatalf("Could not close stream: %v", err)
		        }
		        return
		      }
		      if err != nil {
		        log.Printf("Could not read from file: %v", err)
		        continue
		      }
				}
	    }
		}()

		for {
	    resp, err := stream.Recv()
	    if err == io.EOF {
	      break
	    }
	    if err != nil {
	      log.Fatalf("Cannot stream results: %v", err)
	    }
	    if err := resp.Error; err != nil {
	      log.Fatalf("Could not recognize: %v", err)
	    }
	    for _, result := range resp.Results {
	      fmt.Printf("Result: %+v\n", result)
	    }
	  }
	}

	// Disconnect from the provided voice channel.
	vc.Disconnect()

	return nil
}


func createPionRTPPacket(p *discordgo.Packet) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			Version: 2,
			// Taken from Discord voice docs
			PayloadType:    0x78,
			SequenceNumber: p.Sequence,
			Timestamp:      p.Timestamp,
			SSRC:           p.SSRC,
		},
		Payload: p.Opus,
	}
}

func handleVoice(c chan *discordgo.Packet) []string {
	files := make(map[uint32]media.Writer)
	audioFile := []string{}
	for p := range c {
		file, ok := files[p.SSRC]
		if !ok {
			var err error
			file, err = oggwriter.New(fmt.Sprintf("%d.ogg", p.SSRC), 48000, 2)
			if err != nil {
				fmt.Printf("failed to create file %d.ogg, giving up on recording: %v\n", p.SSRC, err)
				return audioFile
			}
			files[p.SSRC] = file
		}
		// Construct pion RTP packet from DiscordGo's type.
		rtp := createPionRTPPacket(p)
		err := file.WriteRTP(rtp)
		if err != nil {
			fmt.Printf("failed to write to file %d.ogg, giving up on recording: %v\n", p.SSRC, err)
		}
		audioFile = AppendIfMissing(audioFile, fmt.Sprintf("%d.ogg", p.SSRC))
	}

	// Once we made it here, we're done listening for packets. Close all files
	for _, f := range files {
		f.Close()
	}

	return audioFile
}

func AppendIfMissing(slice []string, s string) []string {
    for _, ele := range slice {
        if ele == s {
            return slice
        }
    }
    return append(slice, s)
}
