package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"time"

	"github.com/relistan/go-director"
	log "github.com/sirupsen/logrus"
)

const (
	ClientTimeout  = 3 * time.Second
	DefaultRetries = 5
)

// An UrlListener is an event listener that receives updates over an
// HTTP POST to an endpoint.
type UrlListener struct {
	Url          string
	Retries      int
	Client       *http.Client
	looper       director.Looper
	eventChannel chan ChangeEvent
	managed      bool // Is this to be auto-managed by ServicesState?
	name         string
}

// A StateChangedEvent is sent to UrlListeners when a significant
// event has changed the ServicesState.
type StateChangedEvent struct {
	State       *ServicesState
	ChangeEvent ChangeEvent
}

func prepareCookieJar(listenurl string) *cookiejar.Jar {
	cookieJar, err := cookiejar.New(nil)
	hostname, err2 := os.Hostname()
	cookieUrl, err3 := url.Parse(listenurl)

	if err != nil || err2 != nil || err3 != nil {
		log.Errorf("Failed to prepare HTTP cookie jar for UrlListener(%s)", listenurl)
		return nil
	}

	expiration := time.Now().Add(365 * 24 * time.Hour)
	cookie := &http.Cookie{
		Name:    "sidecar-session-host",
		Value:   hostname + "-" + time.Now().UTC().String(),
		Expires: expiration,
	}

	cookieJar.SetCookies(cookieUrl, []*http.Cookie{cookie})

	return cookieJar
}

func NewUrlListener(listenurl string, managed bool) *UrlListener {
	errorChan := make(chan error, 1)

	// Primarily for the purpose of load balancers that look
	// at a cookie for session affinity.
	cookieJar := prepareCookieJar(listenurl)

	return &UrlListener{
		Url:          listenurl,
		looper:       director.NewFreeLooper(director.FOREVER, errorChan),
		Client:       &http.Client{Timeout: ClientTimeout, Jar: cookieJar},
		eventChannel: make(chan ChangeEvent, 20),
		Retries:      DefaultRetries,
		managed:      managed,
		name:         "UrlListener(" + listenurl + ")",
	}
}

func withRetries(count int, fn func() error) error {
	var result error

	for i := -1; i < count; i++ {
		result = fn()
		if result == nil {
			return nil
		}
		time.Sleep(100 * time.Duration(i) * time.Millisecond)
	}

	log.Warnf("Failed after %d retries", count)
	return result
}

func (u *UrlListener) Name() string {
	return u.name
}

func (u *UrlListener) SetName(name string) {
	u.name = name
}

func (u *UrlListener) Chan() chan ChangeEvent {
	return u.eventChannel
}

func (u *UrlListener) Managed() bool {
	return u.managed
}

func (u *UrlListener) Stop() {
	u.looper.Quit()
}

func (u *UrlListener) Watch(state *ServicesState) {
	state.AddListener(u)

	go func() {
		u.looper.Loop(func() error {
			changedServiceEvent := <-u.eventChannel

			state.RLock()
			event := StateChangedEvent{
				State:       state,
				ChangeEvent: changedServiceEvent,
			}

			data, err := json.Marshal(event)
			state.RUnlock()

			// Check for some kind of junk JSON being generated by state.Encode()
			if err != nil {
				log.Warnf("Skipping post to '%s' because of bad state encoding! (%s)", u.Url, err.Error())
				return nil
			}

			buf := bytes.NewBuffer(data)

			err = withRetries(u.Retries, func() error {
				resp, err := u.Client.Post(u.Url, "application/json", buf)

				if err != nil {
					return err
				}

				if resp.StatusCode > 299 || resp.StatusCode < 200 {
					return fmt.Errorf("Bad status code returned (%d)", resp.StatusCode)
				}

				return nil
			})

			if err != nil {
				log.Warnf("Failed posting state to '%s' %s: %s", u.Url, u.Name(), err.Error())
			}

			return nil
		})
	}()
}
