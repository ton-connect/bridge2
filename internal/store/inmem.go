package store

import (
	"github.com/rs/zerolog/log"
	"sync"
	"time"
	"tonconnect-bridge/internal/bridge"
)

type Memory struct {
	events []*bridge.Event
	mx     sync.Mutex
}

func (m *Memory) Push(event *bridge.Event) bool {
	tm := time.Now().Unix()

	m.mx.Lock()
	defer m.mx.Unlock()

	events := make([]*bridge.Event, 0, len(m.events)+1)
	for _, e := range m.events {
		if e.Deadline-tm > 0 { // cleanup outdated
			events = append(events, e)
		}
	}

	if len(m.events) < 32 {
		m.events = append(m.events, event)
		return true
	}
	return false
}

func (m *Memory) ExecuteAll(lastEventId uint64, exec func(event *bridge.Event) error) error {
	m.mx.Lock()
	defer m.mx.Unlock()

	for _, e := range m.events {
		if e.ID <= lastEventId {
			continue
		}

		if err := exec(e); err != nil {
			return err
		}
		log.Debug().Uint64("id", e.ID).Msg("executed event")
	}
	m.events = nil
	return nil
}
