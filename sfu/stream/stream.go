package stream

import (
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
)

type Stream struct {
	sync.Mutex

	SSRC     uint32
	Receiver *webrtc.RTPReceiver

	ClockRate uint32
	CName     string

	PacketCount  uint32
	LastExpected uint32
	Bitrate      uint64
	LastReport   int64
	LastReceived uint32
	LostRate     float32
	Jitter       float64

	LatestTimestampTime int64
	LatestTimestamp     uint32

	LastSRNTPTime      uint64
	LastSRRTPTime      uint32
	LastSRRecv         int64 // Represents wall clock of the most recent sender report arrival
	BaseSeqNo          uint16
	Cycles             uint32
	LastRtcpPacketTime int64 // Time the last RTCP packet was received.
	LastRtcpSrTime     int64 // Time the last RTCP SR was received. Required for DLSR computation.
	LastTransit        uint32
	MaxSeqNo           uint16 // The highest sequence number received in an RTP data packet
}

func (s *Stream) SetSenderReportData(rtpTime uint32, ntpTime uint64) {
	s.Lock()
	s.LastSRRTPTime = rtpTime
	s.LastSRNTPTime = ntpTime
	s.LastSRRecv = time.Now().UnixNano()
	s.Unlock()
}

func (s *Stream) BuildReceptionReport() rtcp.ReceptionReport {
	extMaxSeq := s.Cycles | uint32(s.MaxSeqNo)
	expected := extMaxSeq - uint32(s.BaseSeqNo) + 1
	lost := expected - s.PacketCount
	if s.PacketCount == 0 {
		lost = 0
	}
	expectedInterval := expected - s.LastExpected
	s.LastExpected = expected

	receivedInterval := s.PacketCount - s.LastReceived
	s.LastReceived = s.PacketCount

	lostInterval := expectedInterval - receivedInterval

	s.LostRate = float32(lostInterval) / float32(expectedInterval)
	var fracLost uint8
	if expectedInterval != 0 && lostInterval > 0 {
		fracLost = uint8((lostInterval << 8) / expectedInterval)
	}
	var dlsr uint32

	if s.LastSRRecv != 0 {
		delayMS := uint32((time.Now().UnixNano() - s.LastSRRecv) / 1e6)
		dlsr = (delayMS / 1e3) << 16
		dlsr |= (delayMS % 1e3) * 65536 / 1000
	}

	rr := rtcp.ReceptionReport{
		SSRC:               s.SSRC,
		FractionLost:       fracLost,
		TotalLost:          lost,
		LastSequenceNumber: extMaxSeq,
		Jitter:             uint32(s.Jitter),
		LastSenderReport:   uint32(s.LastSRNTPTime >> 16),
		Delay:              dlsr,
	}

	return rr
}
