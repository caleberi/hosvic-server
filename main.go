package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v2"
	nk "golang.ngrok.com/ngrok"
	nkcfg "golang.ngrok.com/ngrok/config"
)

const (
	rtcpPLIInterval = time.Second * 1
	ngrokApiKey     = "1Y1BMqNsKVP9a1talr739pg9ELl_4FG2qfAcY1AdqCGnbXxra"
	local           = false
)

// Sdp represent session description protocol describe media communication sessions
type Sdp struct {
	Sdp string
}

func main() {
	log.SetOutput(os.Stdout)

	router := gin.Default()
	router.Use(CORSMiddleware())

	// sender to channel of track
	peerConnectionMap := make(map[string]chan *webrtc.Track)

	m := webrtc.MediaEngine{}

	// Setup the codecs you want to use.
	// Only support VP8(video compression), this makes our proxying code simpler
	m.RegisterCodec(webrtc.NewRTPVP8Codec(webrtc.DefaultPayloadTypeVP8, 90000))

	api := webrtc.NewAPI(webrtc.WithMediaEngine(m))

	peerConnectionConfig := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}

	router.POST("/webrtc/sdp/m/:meetingId/c/:userID/p/:peerId/s/:isSender", func(c *gin.Context) {
		isSender, _ := strconv.ParseBool(c.Param("isSender"))
		userID := c.Param("userID")
		peerID := c.Param("peerId")

		var session Sdp
		if err := c.ShouldBindJSON(&session); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		offer := webrtc.SessionDescription{}
		Decode(session.Sdp, &offer)

		// Create a new RTCPeerConnection
		// this is the gist of webrtc, generates and process SDP
		peerConnection, err := api.NewPeerConnection(peerConnectionConfig)

		if err != nil {
			log.Fatal(err)
		}

		if !isSender {
			log.Println("[GOT MESSAGE FROM SENDER]")
			recieveTrack(peerConnection, peerConnectionMap, peerID)
		} else {
			log.Println("[GOT MESSAGE FROM RECIEVER]")
			createTrack(peerConnection, peerConnectionMap, userID)
		}

		log.Println("[SET REMOTE SDP WITH OFFER]")
		// Set the SessionDescription of remote peer
		peerConnection.SetRemoteDescription(offer)

		// Create answer
		answer, err := peerConnection.CreateAnswer(nil)
		if err != nil {
			log.Fatal(err)
		}

		log.Println("[SET LOCAL SDP WITH ANSWER]")
		// Sets the LocalDescription, and starts our UDP listeners
		err = peerConnection.SetLocalDescription(answer)
		if err != nil {
			log.Fatal(err)
		}

		response := Sdp{Sdp: Encode(answer)}

		log.Printf("[SDP->] %v", response)

		c.JSON(http.StatusOK, response)
	})

	app := &http.Server{
		Addr:           fmt.Sprintf(":%s", "8080"),
		Handler:        router.Handler(),
		ErrorLog:       log.New(os.Stderr, "", log.Ldate|log.Ltime|log.Lshortfile),
		IdleTimeout:    60 * time.Second,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   40 * time.Second,
		MaxHeaderBytes: 1_048_576 * 8,
	}

	shutdown := make(chan error)

	go func(app *http.Server) {
		exit := make(chan os.Signal, 1) // signal channel to notify

		signal.Notify(exit, syscall.SIGINT, syscall.SIGTERM) // listen for SIGTERM and SIGTERM signal

		s := <-exit // on reading from the signal channel once

		log.Printf("caught signal: %s", s.String())

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		shutdown <- app.Shutdown(ctx)

	}(app)

	if local {
		ctx, cancel := context.WithCancel(context.Background())

		defer cancel()

		tunnel, err := nk.Listen(ctx, nkcfg.HTTPEndpoint(
			nkcfg.WithCompression(),
			nkcfg.WithScheme(nkcfg.SchemeHTTP),
			nkcfg.WithScheme(nkcfg.SchemeHTTPS),
		), nk.WithAuthtoken(ngrokApiKey))

		if err != nil {
			app.ErrorLog.Fatalln(err)
		}

		log.Printf("Created tunnel for server on [%s]", tunnel.URL())

		if err := app.Serve(tunnel); errors.Is(err, http.ErrServerClosed) {
			app.ErrorLog.Fatalln(err)
		}
	} else {
		if err := app.ListenAndServe(); errors.Is(err, http.ErrServerClosed) {
			app.ErrorLog.Fatalln(err)
		}
	}

}

// user is the caller of the method
// if user connects before peer: create channel and keep listening till track is added
// if peer connects before user: channel would have been created by peer and track can be added by getting the channel from cache
func recieveTrack(peerConnection *webrtc.PeerConnection,
	peerConnectionMap map[string]chan *webrtc.Track,
	peerID string) {
	if _, ok := peerConnectionMap[peerID]; !ok {
		peerConnectionMap[peerID] = make(chan *webrtc.Track, 1)
	}
	//  request is blocked until connection is established from the recieving user
	localTrack := <-peerConnectionMap[peerID]
	// retreive all the added tracks from the channel and add to the peer-connection to send
	peerConnection.AddTrack(localTrack)
}

// user is the caller of the method
// if user connects before peer: since user is first, user will create the channel and track and will pass the track to the channel
// if peer connects before user: since peer came already, he created the channel and is listning and waiting for me to create and pass track
func createTrack(peerConnection *webrtc.PeerConnection,
	peerConnectionMap map[string]chan *webrtc.Track,
	currentUserID string) {

	if _, err := peerConnection.AddTransceiver(webrtc.RTPCodecTypeVideo); err != nil {
		log.Fatal(err)
	}

	// Set a handler for when a new remote track starts, this just distributes all our packets
	// to connected peers
	peerConnection.OnTrack(func(remoteTrack *webrtc.Track, receiver *webrtc.RTPReceiver) {
		// Send a PLI on an interval so that the publisher is pushing a keyframe every rtcpPLIInterval
		// This can be less wasteful by processing incoming RTCP events, then we would emit a NACK/PLI when a viewer requests it
		go func() {
			ticker := time.NewTicker(rtcpPLIInterval)
			for range ticker.C {
				if rtcpSendErr := peerConnection.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: remoteTrack.SSRC()}}); rtcpSendErr != nil {
					fmt.Println(rtcpSendErr)
				}
			}
		}()

		// Create a local track, all our SFU clients will be fed via this track
		// main track of the broadcaster
		localTrack, newTrackErr := peerConnection.NewTrack(remoteTrack.PayloadType(), remoteTrack.SSRC(), "video", "pion")
		if newTrackErr != nil {
			log.Fatal(newTrackErr)
		}

		// the channel that will have the local track that is used by the sender
		// the localTrack needs to be fed to the reciever
		localTrackChan := make(chan *webrtc.Track, 1)
		localTrackChan <- localTrack
		if existingChan, ok := peerConnectionMap[currentUserID]; ok {
			// feed the exsiting track from user with this track
			existingChan <- localTrack
		} else {
			peerConnectionMap[currentUserID] = localTrackChan
		}

		rtpBuf := make([]byte, 1400)
		for { // for publisher only
			i, readErr := remoteTrack.Read(rtpBuf)
			if readErr != nil {
				log.Fatal(readErr)
			}

			// ErrClosedPipe means we don't have any subscribers, this is ok if no peers have connected yet
			if _, err := localTrack.Write(rtpBuf[:i]); err != nil && err != io.ErrClosedPipe {
				log.Fatal(err)
			}
		}
	})

}
