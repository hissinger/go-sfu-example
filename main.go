package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
	log "github.com/sirupsen/logrus"
)

const (
	// Maximum message size allowed from peer.
	maxMessageSize = 1024 * 9
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

type Client struct {
	conn           *websocket.Conn
	send           chan []byte
	peerConnection *webrtc.PeerConnection
}

type ICECandidate struct {
	Type      string                  `json:"type"`
	Candidate webrtc.ICECandidateInit `json:"candidate"`
}

func (c *Client) readPump() {
	defer func() {
		c.conn.Close()
	}()
	c.conn.SetReadLimit(maxMessageSize)
	for {
		_, byte, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Errorf("error: %v", err)
			}
			break
		}

		var message map[string]interface{}

		if err := json.Unmarshal(byte, &message); err != nil {
			panic(err)
		}

		// rececive message
		// log.Debug(message)

		switch message["type"] {
		case "offer":
			c.receiveOffer(string(byte))
		case "candidate":
			c.receiveCandidate(string(byte))
		}
	}
}

func (c *Client) writePump() {
	defer func() {
		c.conn.Close()
	}()
	for {
		select {
		case message, ok := <-c.send:
			if !ok {
				// The hub closed the channel.
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := c.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(message)

			if err := w.Close(); err != nil {
				return
			}
		}
	}
}

func (c *Client) receiveOffer(message string) {
	log.Debug("message: ", message)

	var offer webrtc.SessionDescription
	if err := json.NewDecoder(strings.NewReader(message)).Decode(&offer); err != nil {
		panic(err)
	}
	if err := c.peerConnection.SetRemoteDescription(offer); err != nil {
		panic(err)
	}

	answer, err := c.peerConnection.CreateAnswer(nil)
	if err != nil {
		panic(err)
	}

	err = c.peerConnection.SetLocalDescription(answer)
	if err != nil {
		panic(err)
	}

	byte, _ := json.Marshal(answer)
	log.Debug("answer:", string(byte))

	c.send <- byte
}

func (c *Client) receiveCandidate(message string) {
	log.Debug("candidate: ", message)

	var candidate = webrtc.ICECandidateInit{Candidate: message}

	if err := c.peerConnection.AddICECandidate(candidate); err != nil {
		panic(err)
	}
}

func (c *Client) peerConnectionFactory() {

	api := webrtc.NewAPI(webrtc.WithMediaEngine(m))

	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
		SDPSemantics: webrtc.SDPSemanticsUnifiedPlan,
	})
	if err != nil {
		panic(err)
	}

	_, err = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo)
	if err != nil {
		panic(err)
	}

	_, err = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio)
	if err != nil {
		panic(err)
	}

	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}

		log.Debug("OnICECandidate:", candidate.ToJSON().Candidate)

		byte, _ := json.Marshal(ICECandidate{
			Type:      "candidate",
			Candidate: candidate.ToJSON(),
		})

		c.send <- byte
	})

	pc.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		log.Debugf("Connection State has changed %s \n", connectionState.String())
	})

	pc.OnTrack(func(remoteTrack *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {

		var bps uint64

		go func() {
			for {
				log.Debug("bps:", bps*8)
				bps = 0
				time.Sleep(time.Second * 1)
			}
		}()

		rtpBuf := make([]byte, 1400)
		for {
			i, _, readErr := remoteTrack.Read(rtpBuf)
			if readErr != nil {
				log.Fatalln(err)
			}

			// log.Debugf("type: %d len: %d", remoteTrack.Kind(), i)
			bps += uint64(i)
		}

	})

	c.peerConnection = pc
}

func serveWs(w http.ResponseWriter, r *http.Request) {
	upgrader.CheckOrigin = func(r *http.Request) bool { return true }
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Print("upgrade:", err)
		return
	}

	client := &Client{
		conn: c,
		send: make(chan []byte, 256),
	}

	client.peerConnectionFactory()

	go client.writePump()
	go client.readPump()
}

var m *webrtc.MediaEngine

func main() {

	log.SetOutput(os.Stdout)
	log.SetLevel(log.DebugLevel)

	m = &webrtc.MediaEngine{}
	m.RegisterDefaultCodecs()

	http.HandleFunc("/ws", serveWs)
	err := http.ListenAndServe("localhost:8080", nil)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}
