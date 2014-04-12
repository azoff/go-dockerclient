// Copyright 2013 go-dockerclient authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package docker

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type APIEvents struct {
	Status string
	ID     string
	From   string
	Time   int64
}

type eventMonitoringState struct {
	sync.RWMutex
	sync.WaitGroup
	enabled   bool
	lastSeen  *int64
	C         chan *APIEvents
	errC      chan error
	listeners []chan *APIEvents
}

const (
	maxMonitorConnRetries = 5
	retryInitialWaitTime  = 10.
)

var (
	eventMonitor      eventMonitoringState
	ErrNoListeners    = errors.New("No listeners present to recieve event")
	ErrListenerExists = errors.New("Listener already exists for docker events")
)

func (c *Client) AddEventListener(listener chan *APIEvents) error {
	var err error
	if !eventMonitor.isEnabled() {
		err = eventMonitor.enableEventMonitoring(c)
		if err != nil {
			return err
		}
	}
	err = eventMonitor.addListener(listener)
	if err != nil {
		return err
	}
	return nil
}

func (c *Client) RemoveEventListener(listener chan *APIEvents) error {
	err := eventMonitor.removeListener(listener)
	if err != nil {
		return err
	}

	if len(eventMonitor.listeners) == 0 {
		err = eventMonitor.disableEventMonitoring()
		if err != nil {
			return err
		}
	}

	return nil
}

func (eventState *eventMonitoringState) addListener(listener chan *APIEvents) error {
	eventState.Lock()
	defer eventState.Unlock()
	if listenerExists(listener, &eventState.listeners) {
		return ErrListenerExists
	}
	eventState.Add(1)
	eventState.listeners = append(eventState.listeners, listener)
	return nil
}

func (eventState *eventMonitoringState) removeListener(listener chan *APIEvents) error {
	eventState.Lock()
	defer eventState.Unlock()
	if listenerExists(listener, &eventState.listeners) {
		var newListeners []chan *APIEvents
		for _, l := range eventState.listeners {
			if l != listener {
				newListeners = append(newListeners, l)
			}
		}
		eventState.listeners = newListeners
		eventState.Add(-1)
	}
	return nil
}

func listenerExists(a chan *APIEvents, list *[]chan *APIEvents) bool {
	for _, b := range *list {
		if b == a {
			return true
		}
	}
	return false
}

func (eventState *eventMonitoringState) enableEventMonitoring(c *Client) error {
	eventState.Lock()
	defer eventState.Unlock()
	if !eventState.enabled {
		eventState.enabled = true
		var lastSeenDefault = int64(0)
		eventState.lastSeen = &lastSeenDefault
		eventState.C = make(chan *APIEvents, 100)
		eventState.errC = make(chan error, 1)
		go eventState.monitorEvents(c)
	}
	return nil
}

func (eventState *eventMonitoringState) disableEventMonitoring() error {
	eventState.Wait()
	eventState.Lock()
	defer eventState.Unlock()
	if eventState.enabled {
		eventState.enabled = false
		close(eventState.C)
		close(eventState.errC)
	}
	return nil
}

func (eventState *eventMonitoringState) monitorEvents(c *Client) {
	var err error
	for eventState.noListeners() {
		time.Sleep(10 * time.Millisecond)
	}
	if err = eventState.connectWithRetry(c); err != nil {
		eventState.terminate(err)
	}
	for eventState.isEnabled() {
		timeout := time.After(100 * time.Millisecond)
		select {
		case ev, ok := <-eventState.C:
			if !ok {
				return
			}
			go eventState.sendEvent(ev)
			go eventState.updateLastSeen(ev)

		case err = <-eventState.errC:
			if err == ErrNoListeners {
				eventState.terminate(nil)
				return
			} else if err != nil {
				defer func() { go eventState.monitorEvents(c) }()
				return
			}
		case <-timeout:
			continue
		}
	}
}

func (eventState *eventMonitoringState) connectWithRetry(c *Client) error {
	var retries int
	var err error
	for err = c.eventHijack(atomic.LoadInt64(eventState.lastSeen), eventState.C, eventState.errC); err != nil && retries < maxMonitorConnRetries; retries++ {
		waitTime := int64(retryInitialWaitTime * math.Pow(2, float64(retries)))
		time.Sleep(time.Duration(waitTime) * time.Millisecond)
		err = c.eventHijack(atomic.LoadInt64(eventState.lastSeen), eventState.C, eventState.errC)
	}
	return err
}

func (eventState *eventMonitoringState) noListeners() bool {
	eventState.RLock()
	defer eventState.RUnlock()
	return len(eventState.listeners) == 0
}

func (eventState *eventMonitoringState) isEnabled() bool {
	eventState.RLock()
	defer eventState.RUnlock()
	return eventState.enabled
}

func (eventState *eventMonitoringState) sendEvent(event *APIEvents) {
	eventState.RLock()
	defer eventState.RUnlock()
	eventState.Add(1)
	defer eventState.Done()
	if eventState.isEnabled() {
		if eventState.noListeners() {
			eventState.errC <- ErrNoListeners
		}
		for _, listener := range eventState.listeners {
			listener <- event
		}
	}
}

func (eventState *eventMonitoringState) updateLastSeen(e *APIEvents) {
	eventState.Lock()
	defer eventState.Unlock()
	if atomic.LoadInt64(eventState.lastSeen) < e.Time {
		atomic.StoreInt64(eventState.lastSeen, e.Time)
	}
}

func (eventState *eventMonitoringState) terminate(err error) {
	eventState.disableEventMonitoring()
}

func (c *Client) eventHijack(startTime int64, eventChan chan *APIEvents, errChan chan error) error {
	uri := "/events"
	if startTime != 0 {
		uri += fmt.Sprintf("?since=%d", startTime)
	}
	req, err := http.NewRequest("GET", c.getURL(uri), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "plain/text")
	protocol := c.endpointURL.Scheme
	address := c.endpointURL.Path
	if protocol != "unix" {
		protocol = "tcp"
		address = c.endpointURL.Host
	}
	dial, err := net.Dial(protocol, address)
	if err != nil {
		return err
	}
	clientconn := httputil.NewClientConn(dial, nil)
	clientconn.Do(req)
	conn, rwc := clientconn.Hijack()
	if err != nil {
		return err
	}
	go func(rwc io.Reader) {
		defer clientconn.Close()
		defer conn.Close()
		scanner := bufio.NewScanner(rwc)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "{") {
				var e APIEvents
				err = json.Unmarshal([]byte(line), &e)
				if err != nil {
					errChan <- err
				}
				eventChan <- &e
			}
		}
		if err := scanner.Err(); err != nil {
			errChan <- err
		}
	}(rwc)
	return nil
}