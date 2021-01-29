package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"github.com/sirupsen/logrus"
	log "github.com/sirupsen/logrus"
)

const (
	maxSeqNo       = 1 << 16
	maxMessageSize = 1024 * 9
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

type stream struct {
	sync.Mutex

	ssrc     uint32
	receiver *webrtc.RTPReceiver

	clockRate uint32
	cname     string

	packetCount  uint32
	lastExpected uint32
	bitrate      uint64
	lastReport   int64
	lastReceived uint32
	lostRate     float32
	jitter       float64

	latestTimestampTime int64
	latestTimestamp     uint32

	lastSRNTPTime      uint64
	lastSRRTPTime      uint32
	lastSRRecv         int64 // Represents wall clock of the most recent sender report arrival
	baseSeqNo          uint16
	cycles             uint32
	lastRtcpPacketTime int64 // Time the last RTCP packet was received.
	lastRtcpSrTime     int64 // Time the last RTCP SR was received. Required for DLSR computation.
	lastTransit        uint32
	maxSeqNo           uint16 // The highest sequence number received in an RTP data packet
}

type Client struct {
	conn           *websocket.Conn
	send           chan []byte
	peerConnection *webrtc.PeerConnection

	receiveStreams map[uint32]*stream
	ctx            context.Context
	ctxCancel      context.CancelFunc
}

type ICECandidate struct {
	Type      string                  `json:"type"`
	Candidate webrtc.ICECandidateInit `json:"candidate"`
}

func (c *Client) readPump() {
	defer func() {
		c.conn.Close()
		log.Info("exit readPump goroutine")
	}()
	c.conn.SetReadLimit(maxMessageSize)
	for {
		_, byte, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Errorf("error: %v", err)
			}
			c.Close()
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

func (c *Client) Close() {
	c.peerConnection.Close()
	c.ctxCancel()
}

func (c *Client) writePump() {
	defer func() {
		c.conn.Close()
		log.Info("exit writePump goroutine")
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

		ssrc := uint32(remoteTrack.SSRC())
		stream := &stream{
			receiver:  receiver,
			ssrc:      ssrc,
			clockRate: remoteTrack.Codec().ClockRate,
		}

		log.Println("clock:", remoteTrack.Codec().ClockRate)
		c.receiveStreams[ssrc] = stream

		go func() {
			ticker := time.NewTicker(1 * time.Second)
			defer func() {
				ticker.Stop()
				log.Info("exit bitrate goroutine")
			}()

			for {
				select {
				case <-c.ctx.Done():
					return
				case <-ticker.C:
					log.Debug("bps:", stream.bitrate)
					stream.bitrate = 0
				}
			}
		}()

		go func() {
			ticker := time.NewTicker(1 * time.Second)
			defer func() {
				ticker.Stop()
				log.Info("exit WriteRTCP goroutine")
			}()

			for {
				select {
				case <-c.ctx.Done():
					return
				case <-ticker.C:
					rtcp := []rtcp.Packet{c.buildREMBPacket()}
					pc.WriteRTCP(rtcp)
				}
			}
		}()

		go func() {
			defer func() {
				log.Info("exit readRTCP goroutine")
			}()
			for {
				pkts, _, readErr := receiver.ReadRTCP()
				if readErr != nil {
					log.Warn("read RTCP:", readErr)
					return
				}

				for _, pkt := range pkts {
					switch pkt.(type) {
					case *rtcp.SourceDescription:
						c.handleSDES(pkt.(*rtcp.SourceDescription))
					case *rtcp.SenderReport:
						c.handleSR(pkt.(*rtcp.SenderReport))
					case *rtcp.ReceiverReport:
						c.handleRR(pkt.(*rtcp.ReceiverReport))
					case *rtcp.Goodbye:
						c.handleBye(pkt.(*rtcp.Goodbye))
					default:
						log.Println("unknown RTCP Type:", pkt)
					}
				}
			}
		}()

		go func() {
			defer func() {
				log.Info("exit readRTP goroutine")
			}()
			pkt := make([]byte, 1500)
			for {
				i, _, readErr := remoteTrack.Read(pkt)
				if readErr != nil {
					log.Warn("read RTP:", readErr)
					return
				}
				arrivalTime := time.Now().UnixNano()

				seq := binary.BigEndian.Uint16(pkt[2:4])
				if stream.packetCount == 0 {
					stream.baseSeqNo = seq
					stream.maxSeqNo = seq
					stream.lastReport = arrivalTime
				} else if (seq-stream.maxSeqNo)&0x8000 == 0 {
					if seq < stream.maxSeqNo {
						stream.cycles += maxSeqNo
					}
					stream.maxSeqNo = seq
				}
				stream.packetCount++
				stream.bitrate += (uint64(i) * 8)

				var p rtp.Packet
				if err := p.Unmarshal(pkt); err != nil {
					return
				}

				if (stream.latestTimestampTime == 0) || IsLaterTimestamp(p.Timestamp, stream.latestTimestamp) {
					stream.latestTimestamp = p.Timestamp
					stream.latestTimestampTime = arrivalTime
				}

				arrival := uint32(arrivalTime / 1e6 * int64(stream.clockRate/1e3))
				transit := arrival - p.Timestamp

				if stream.lastTransit != 0 {
					d := int32(transit - stream.lastTransit)
					if d < 0 {
						d = -d
					}
					stream.jitter += (float64(d) - stream.jitter) / 16
				}

				stream.lastTransit = transit
			}
		}()
	})

	c.peerConnection = pc
}

// IsLaterTimestamp returns true if timestamp1 is later in time than timestamp2 factoring in timestamp wrap-around
func IsLaterTimestamp(nowTimestamp uint32, latestTimestamp uint32) bool {
	if nowTimestamp > latestTimestamp {
		if IsTimestampWrapAround(latestTimestamp, nowTimestamp) {
			return false
		}
		return true
	}
	if IsTimestampWrapAround(nowTimestamp, latestTimestamp) {
		return true
	}
	return false
}

// IsTimestampWrapAround returns true if wrap around happens from timestamp1 to timestamp2
func IsTimestampWrapAround(nowTimestamp uint32, latestTimestamp uint32) bool {
	return (nowTimestamp&0xC000000 == 0) && (latestTimestamp&0xC000000 == 0xC000000)
}

func (c *Client) handleSR(sr *rtcp.SenderReport) {
	for _, ssrc := range sr.DestinationSSRC() {
		// log.Println("receive SR:", ssrc)
		s, ok := c.receiveStreams[ssrc]
		if ok == true {
			// log.Println(s.receiver.Track().Kind())
			// log.Println(sr.String())
			s.SetSenderReportData(sr.RTPTime, sr.NTPTime)

			rtcp := []rtcp.Packet{
				&rtcp.ReceiverReport{
					Reports: []rtcp.ReceptionReport{s.buildReceptionReport()},
				},
			}
			c.peerConnection.WriteRTCP(rtcp)
		}
	}
}

func (s *stream) SetSenderReportData(rtpTime uint32, ntpTime uint64) {
	s.Lock()
	s.lastSRRTPTime = rtpTime
	s.lastSRNTPTime = ntpTime
	s.lastSRRecv = time.Now().UnixNano()
	s.Unlock()
}

func (c *Client) handleRR(rr *rtcp.ReceiverReport) {
	for _, r := range rr.Reports {
		log.Println(r.SSRC)
		log.Println(r.FractionLost)
		log.Println(r.TotalLost)
		log.Println(r.LastSequenceNumber)
		log.Println(r.Jitter)
		log.Println(r.LastSenderReport)
		log.Println(r.Delay)
	}
}

func (c *Client) handleSDES(sdes *rtcp.SourceDescription) {
	for _, chunk := range sdes.Chunks {
		for _, item := range chunk.Items {
			if item.Type == rtcp.SDESCNAME {
				s := c.receiveStreams[chunk.Source]
				s.Lock()
				defer s.Unlock()
				s.cname = item.Text
			}
		}
	}
}

func (c *Client) handleBye(bye *rtcp.Goodbye) {
	for _, src := range bye.Sources {
		log.Printf("BYE: ssrc:%d reason:%s", src, bye.Reason)
	}
}

func (s *stream) buildReceptionReport() rtcp.ReceptionReport {
	extMaxSeq := s.cycles | uint32(s.maxSeqNo)
	expected := extMaxSeq - uint32(s.baseSeqNo) + 1
	lost := expected - s.packetCount
	if s.packetCount == 0 {
		lost = 0
	}
	expectedInterval := expected - s.lastExpected
	s.lastExpected = expected

	receivedInterval := s.packetCount - s.lastReceived
	s.lastReceived = s.packetCount

	lostInterval := expectedInterval - receivedInterval

	s.lostRate = float32(lostInterval) / float32(expectedInterval)
	var fracLost uint8
	if expectedInterval != 0 && lostInterval > 0 {
		fracLost = uint8((lostInterval << 8) / expectedInterval)
	}
	var dlsr uint32

	if s.lastSRRecv != 0 {
		delayMS := uint32((time.Now().UnixNano() - s.lastSRRecv) / 1e6)
		dlsr = (delayMS / 1e3) << 16
		dlsr |= (delayMS % 1e3) * 65536 / 1000
	}

	rr := rtcp.ReceptionReport{
		SSRC:               s.ssrc,
		FractionLost:       fracLost,
		TotalLost:          lost,
		LastSequenceNumber: extMaxSeq,
		Jitter:             uint32(s.jitter),
		LastSenderReport:   uint32(s.lastSRNTPTime >> 16),
		Delay:              dlsr,
	}

	return rr
}

func (c *Client) buildREMBPacket() *rtcp.ReceiverEstimatedMaximumBitrate {
	br := uint64(2 * 1024 * 1024)

	for _, r := range c.peerConnection.GetReceivers() {
		if r.Track().Kind() == webrtc.RTPCodecTypeVideo {
			return &rtcp.ReceiverEstimatedMaximumBitrate{
				Bitrate: br,
				SSRCs:   []uint32{uint32(r.Track().SSRC())},
			}
		}
	}
	return nil
}

func serveWs(w http.ResponseWriter, r *http.Request) {
	upgrader.CheckOrigin = func(r *http.Request) bool { return true }
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Print("upgrade:", err)
		return
	}

	client := &Client{
		conn:           c,
		send:           make(chan []byte, 256),
		receiveStreams: map[uint32]*stream{},
	}

	client.ctx, client.ctxCancel = context.WithCancel(context.Background())

	client.peerConnectionFactory()

	go client.writePump()
	go client.readPump()
}

var m *webrtc.MediaEngine

func main() {

	// setting logrus
	log.SetOutput(os.Stdout)
	log.SetLevel(log.DebugLevel)
	log.SetReportCaller(true)
	formatter := &logrus.TextFormatter{
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
