package catalog

import (
	"encoding/json"
	"io/ioutil"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/Nitro/sidecar/service"
	"github.com/relistan/go-director"
	. "github.com/smartystreets/goconvey/convey"
	"gopkg.in/jarcoal/httpmock.v1"
)

func Test_NewUrlListener(t *testing.T) {
	Convey("NewUrlListener() configures all the right things", t, func() {
		url := "http://beowulf.example.com"
		listener := NewUrlListener(url, false)

		So(listener.Client, ShouldNotBeNil)
		So(listener.Url, ShouldEqual, url)
		So(listener.looper, ShouldNotBeNil)
	})
}

func Test_prepareCookieJar(t *testing.T) {
	Convey("When preparing the cookie jar", t, func() {
		listenurl := "http://beowulf.example.com/"

		Convey("We get a properly generated cookie for our url", func() {
			jar := prepareCookieJar(listenurl)
			cookieUrl, _ := url.Parse(listenurl)
			cookies := jar.Cookies(cookieUrl)

			So(len(cookies), ShouldEqual, 1)
			So(cookies[0].Value, ShouldNotBeEmpty)
			So(cookies[0].Expires, ShouldNotBeEmpty)
		})

		Convey("We only get the right cookies", func() {
			jar := prepareCookieJar(listenurl)
			wrongUrl, _ := url.Parse("http://wrong.example.com")
			cookies := jar.Cookies(wrongUrl)

			So(len(cookies), ShouldEqual, 0)
		})
	})
}

func Test_Listen(t *testing.T) {
	Convey("Listen()", t, func(c C) {
		url := "http://beowulf.example.com"
		hostname := "grendel"

		svcId1 := "deadbeef123"
		service1 := service.Service{ID: svcId1, Hostname: hostname}
		svcId2 := "ecgtheow"
		service2 := service.Service{ID: svcId2, Hostname: hostname}

		state := NewServicesState()
		state.Hostname = hostname

		postShouldErr := false
		var changeEventTime time.Time
		httpmock.RegisterResponder(
			"POST", url,
			func(req *http.Request) (*http.Response, error) {
				if postShouldErr {
					return httpmock.NewStringResponse(500, "so bad!"), nil
				}

				bodyBytes, err := ioutil.ReadAll(req.Body)
				c.So(err, ShouldBeNil)

				var evt StateChangedEvent
				err = json.Unmarshal(bodyBytes, &evt)
				c.So(err, ShouldBeNil)
				c.So(evt.ChangeEvent.PreviousStatus, ShouldEqual, service.ALIVE)

				// Make sure each new event comes in with a different timestamp
				c.So(evt.ChangeEvent.Time, ShouldNotEqual, changeEventTime)
				changeEventTime = evt.ChangeEvent.Time

				return httpmock.NewBytesResponse(200, nil), nil
			},
		)
		httpmock.Activate()
		Reset(func() {
			httpmock.DeactivateAndReset()
		})

		Convey("handles a bad post", func() {
			postShouldErr = true

			state.AddServiceEntry(service1)
			state.Servers[hostname].Services[service1.ID].Tombstone()

			listener := NewUrlListener(url, false)
			errors := make(chan error)
			listener.looper = director.NewFreeLooper(1, errors)

			listener.eventChannel <- ChangeEvent{}
			listener.Retries = 0
			listener.Watch(state)
			err := listener.looper.Wait()

			So(err, ShouldBeNil)
			So(len(errors), ShouldEqual, 0)
		})

		Convey("gets all updates when a server expires", func() {
			state.AddServiceEntry(service1)
			state.AddServiceEntry(service2)

			listener := NewUrlListener(url, false)
			errors := make(chan error)
			// Do two iterations: One for each service from the expired server
			listener.looper = director.NewFreeLooper(
				len(state.Servers[hostname].Services), errors)
			listener.Retries = 0

			listener.Watch(state)

			state.ExpireServer(hostname)

			// Block until both iterations are done
			err := listener.looper.Wait()
			So(err, ShouldBeNil)
			So(len(errors), ShouldEqual, 0)
		})
	})
}
