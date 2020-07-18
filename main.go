package main

import (
  "context"
  "fmt"
  "github.com/bwmarrin/discordgo"
  "github.com/bwmarrin/dgvoice"
  "io/ioutil"
  "io"
  "log"
  "os"
	"os/signal"
  "syscall"
  speech "cloud.google.com/go/speech/apiv1"
  speechpb "google.golang.org/genproto/googleapis/cloud/speech/v1"
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
  stream, err := client.StreamingRecognize(ctx)
  if err != nil {
    log.Fatal(err)
  }
  // Send the initial configuration message.
  if err := stream.Send(&speechpb.StreamingRecognizeRequest{
    StreamingRequest: &speechpb.StreamingRecognizeRequest_StreamingConfig{
      StreamingConfig: &speechpb.StreamingRecognitionConfig{
        Config: &speechpb.RecognitionConfig{
          Encoding:        speechpb.RecognitionConfig_OGG_OPUS,
          SampleRateHertz: 48000,
          AudioChannelCount: 2,
          LanguageCode:    "en-US",
        },
      },
    },
  }); err != nil {
    log.Fatal(err)
  }

  go func() {
    // Pipe stdin to the API.
    for i := 0; i < 250; i++ {
      recv := make(chan *discordgo.Packet, 2)
    	go dgvoice.ReceivePCM(vc, recv)
      r := <-recv
      if err := stream.Send(&speechpb.StreamingRecognizeRequest{
        StreamingRequest: &speechpb.StreamingRecognizeRequest_AudioContent{
          AudioContent: r.Opus,
        },
      }); err != nil {
        log.Printf("Could not send audio: %v", err)
      }
      if err == io.EOF {
        // Nothing else to pipe, close the stream.
        if err := stream.CloseSend(); err != nil {
          log.Fatalf("Could not close stream: %v", err)
        }
        log.Printf("stream closed")
        return
      }
      print(i)
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
      // Workaround while the API doesn't give a more informative error.
      if err.Code == 3 || err.Code == 11 {
        log.Print("WARNING: Speech recognition request exceeded limit of 60 seconds.")
      }
      log.Fatalf("Could not recognize: %v", err)
    }
    for _, result := range resp.Results {
    fmt.Printf("Result: %+v\n", result)
    }
  }

	// Disconnect from the provided voice channel.
	vc.Disconnect()

	return nil
}
