package main

import (
	"context"
	"fmt"
	"go-video-conference/stream"
	"go-video-conference/user"
	"net/http"
	"os"
	"path/filepath"
	"runtime"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
	log "github.com/sirupsen/logrus"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

var m *webrtc.MediaEngine

func serveWs(w http.ResponseWriter, r *http.Request) {
	upgrader.CheckOrigin = func(r *http.Request) bool { return true }
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Print("upgrade:", err)
		return
	}

	client := &user.User{
		MediaEngine:    m,
		Conn:           c,
		Send:           make(chan []byte, 256),
		ReceiveStreams: map[uint32]*stream.Stream{},
	}

	client.Ctx, client.CtxCancel = context.WithCancel(context.Background())

	client.PeerConnectionFactory()

	go client.WritePump()
	go client.ReadPump()
}

func main() {

	// setting logrus
	log.SetOutput(os.Stdout)
	log.SetLevel(log.DebugLevel)
	log.SetReportCaller(true)
	formatter := &log.TextFormatter{
		CallerPrettyfier: func(f *runtime.Frame) (string, string) {
			filename := filepath.Base(f.File)
			return "", fmt.Sprint(" ", filename, ":", f.Line)
		},
		TimestampFormat: "20060102-150405.000000",
		FullTimestamp:   true,
		ForceColors:     true,
	}
	log.SetFormatter(formatter)

	m = &webrtc.MediaEngine{}
	m.RegisterDefaultCodecs()

	http.HandleFunc("/ws", serveWs)
	err := http.ListenAndServe("localhost:8080", nil)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}
