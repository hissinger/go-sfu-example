package user

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"strings"
	"time"

	"go-video-conference/stream"

	"github.com/gorilla/websocket"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	log "github.com/sirupsen/logrus"
)

const (
	maxSeqNo       = 1 << 16
	maxMessageSize = 1024 * 9
)

type ICECandidate struct {
	Type      string                  `json:"type"`
	Candidate webrtc.ICECandidateInit `json:"candidate"`
}

type User struct {
	MediaEngine    *webrtc.MediaEngine
	Conn           *websocket.Conn
	Send           chan []byte
	PeerConnection *webrtc.PeerConnection

	ReceiveStreams map[uint32]*stream.Stream
	Ctx            context.Context
	CtxCancel      context.CancelFunc
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

func (c *User) Close() {
	c.PeerConnection.Close()
	c.CtxCancel()
}

func (c *User) ReadPump() {
	defer func() {
		c.Conn.Close()
		log.Info("exit readPump goroutine")
	}()
	c.Conn.SetReadLimit(maxMessageSize)
	for {
		_, byte, err := c.Conn.ReadMessage()
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

func (c *User) WritePump() {
	defer func() {
		c.Conn.Close()
		log.Info("exit writePump goroutine")
	}()
	for {
		select {
		case <-c.Ctx.Done():
			return
		case message, ok := <-c.Send:
			if !ok {
				// The hub closed the channel.
				c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := c.Conn.NextWriter(websocket.TextMessage)
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

func (c *User) receiveOffer(message string) {
	log.Debug("message: ", message)

	var offer webrtc.SessionDescription
	if err := json.NewDecoder(strings.NewReader(message)).Decode(&offer); err != nil {
		panic(err)
	}
	if err := c.PeerConnection.SetRemoteDescription(offer); err != nil {
		panic(err)
	}

	answer, err := c.PeerConnection.CreateAnswer(nil)
	if err != nil {
		panic(err)
	}

	err = c.PeerConnection.SetLocalDescription(answer)
	if err != nil {
		panic(err)
	}

	byte, _ := json.Marshal(answer)
	log.Debug("answer:", string(byte))

	c.Send <- byte
}

func (c *User) receiveCandidate(message string) {
	log.Debug("candidate: ", message)

	var candidate = webrtc.ICECandidateInit{Candidate: message}

	if err := c.PeerConnection.AddICECandidate(candidate); err != nil {
		panic(err)
	}
}

func (c *User) PeerConnectionFactory() {

	api := webrtc.NewAPI(webrtc.WithMediaEngine(c.MediaEngine))

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

		c.Send <- byte
	})

	pc.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		log.Debugf("Connection State has changed %s \n", connectionState.String())
	})

	pc.OnTrack(func(remoteTrack *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {

		ssrc := uint32(remoteTrack.SSRC())
		stream := &stream.Stream{
			Receiver:  receiver,
			SSRC:      ssrc,
			ClockRate: remoteTrack.Codec().ClockRate,
		}

		log.Println("clock:", remoteTrack.Codec().ClockRate)
		c.ReceiveStreams[ssrc] = stream

		go func() {
			ticker := time.NewTicker(1 * time.Second)
			defer func() {
				ticker.Stop()
				log.Info("exit bitrate goroutine")
			}()

			for {
				select {
				case <-c.Ctx.Done():
					return
				case <-ticker.C:
					log.Debug("bps:", stream.Bitrate)
					stream.Bitrate = 0
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
				case <-c.Ctx.Done():
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
				if stream.PacketCount == 0 {
					stream.BaseSeqNo = seq
					stream.MaxSeqNo = seq
					stream.LastReport = arrivalTime
				} else if (seq-stream.MaxSeqNo)&0x8000 == 0 {
					if seq < stream.MaxSeqNo {
						stream.Cycles += maxSeqNo
					}
					stream.MaxSeqNo = seq
				}
				stream.PacketCount++
				stream.Bitrate += (uint64(i) * 8)

				var p rtp.Packet
				if err := p.Unmarshal(pkt); err != nil {
					return
				}

				if (stream.LatestTimestampTime == 0) || IsLaterTimestamp(p.Timestamp, stream.LatestTimestamp) {
					stream.LatestTimestamp = p.Timestamp
					stream.LatestTimestampTime = arrivalTime
				}

				arrival := uint32(arrivalTime / 1e6 * int64(stream.ClockRate/1e3))
				transit := arrival - p.Timestamp

				if stream.LastTransit != 0 {
					d := int32(transit - stream.LastTransit)
					if d < 0 {
						d = -d
					}
					stream.Jitter += (float64(d) - stream.Jitter) / 16
				}

				stream.LastTransit = transit
			}
		}()
	})

	c.PeerConnection = pc
}

func (c *User) handleRR(rr *rtcp.ReceiverReport) {
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

func (c *User) handleSDES(sdes *rtcp.SourceDescription) {
	for _, chunk := range sdes.Chunks {
		for _, item := range chunk.Items {
			if item.Type == rtcp.SDESCNAME {
				s := c.ReceiveStreams[chunk.Source]
				s.Lock()
				defer s.Unlock()
				s.CName = item.Text
			}
		}
	}
}

func (c *User) handleBye(bye *rtcp.Goodbye) {
	for _, src := range bye.Sources {
		log.Printf("BYE: ssrc:%d reason:%s", src, bye.Reason)
	}
}

func (c *User) buildREMBPacket() *rtcp.ReceiverEstimatedMaximumBitrate {
	br := uint64(2 * 1024 * 1024)

	for _, r := range c.PeerConnection.GetReceivers() {
		if r.Track().Kind() == webrtc.RTPCodecTypeVideo {
			return &rtcp.ReceiverEstimatedMaximumBitrate{
				Bitrate: br,
				SSRCs:   []uint32{uint32(r.Track().SSRC())},
			}
		}
	}
	return nil
}

func (c *User) handleSR(sr *rtcp.SenderReport) {
	for _, ssrc := range sr.DestinationSSRC() {
		// log.Println("receive SR:", ssrc)
		s, ok := c.ReceiveStreams[ssrc]
		if ok == true {
			// log.Println(s.receiver.Track().Kind())
			// log.Println(sr.String())
			s.SetSenderReportData(sr.RTPTime, sr.NTPTime)

			rtcp := []rtcp.Packet{
				&rtcp.ReceiverReport{
					Reports: []rtcp.ReceptionReport{s.BuildReceptionReport()},
				},
			}
			c.PeerConnection.WriteRTCP(rtcp)
		}
	}
}
