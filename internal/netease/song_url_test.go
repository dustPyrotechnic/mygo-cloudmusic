package netease

import (
	"bytes"
	"crypto/aes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type eapiTestTransport func(*http.Request) (*http.Response, error)

func (f eapiTestTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func songURLServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(handler)
	target, _ := url.Parse(server.URL)
	transport := &http.Transport{}
	previous := http.DefaultTransport
	previousJar := httpClient.Jar
	httpClient.Jar, _ = cookiejar.New(nil)
	http.DefaultTransport = eapiTestTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "interfacepc.music.163.com" || req.URL.Path != "/eapi/song/enhance/player/url/v1" {
			return nil, fmt.Errorf("unexpected request: %s", req.URL.Host+req.URL.Path)
		}
		forward := req.Clone(req.Context())
		forward.URL.Scheme, forward.URL.Host = target.Scheme, target.Host
		return transport.RoundTrip(forward)
	})
	deviceOnce.Do(func() { deviceID = strings.Repeat("A", 52) })
	t.Cleanup(func() {
		http.DefaultTransport = previous
		httpClient.Jar = previousJar
		transport.CloseIdleConnections()
		server.Close()
	})
}

type songURLRequest struct {
	IDs        string `json:"ids"`
	Level      string `json:"level"`
	EncodeType string `json:"encodeType"`
	Header     struct {
		OS         string `json:"os"`
		AppVersion string `json:"appver"`
	} `json:"header"`
}

func decodeSongURLRequest(r *http.Request) (songURLRequest, error) {
	var request songURLRequest
	if err := r.ParseForm(); err != nil {
		return request, err
	}
	ciphertext, err := hex.DecodeString(r.Form.Get("params"))
	if err != nil || len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return request, errors.New("invalid EAPI ciphertext")
	}
	block, _ := aes.NewCipher([]byte(eapiKey))
	plain := make([]byte, len(ciphertext))
	for i := 0; i < len(ciphertext); i += aes.BlockSize {
		block.Decrypt(plain[i:i+aes.BlockSize], ciphertext[i:i+aes.BlockSize])
	}
	parts := bytes.Split(plain, []byte("-36cd479b6b5-"))
	if len(parts) != 3 {
		return request, errors.New("invalid EAPI envelope")
	}
	err = json.Unmarshal(parts[1], &request)
	return request, err
}

const songURLSuccess = `{"code":200,"data":[{"id":42,"code":200,"url":"http://audio.example/song.flac","type":"flac","level":"hires","size":12345,"freeTrialInfo":null}]}`

func TestSongURLUsesConsistentDesktopIdentity(t *testing.T) {
	songURLServer(t, func(w http.ResponseWriter, r *http.Request) {
		request, err := decodeSongURLRequest(r)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if request.Header.OS != "pc" || !strings.Contains(r.UserAgent(), "NeteaseMusicDesktop/"+request.Header.AppVersion) {
			io.WriteString(w, `{"code":-460,"message":"client identity mismatch"}`)
			return
		}
		io.WriteString(w, songURLSuccess)
	})
	stream, err := SongURL(42, "hires")
	if err != nil || stream == nil {
		t.Fatalf("desktop stream rejected: %v", err)
	}
	if stream.URL != "https://audio.example/song.flac" || stream.Level != "hires" || stream.Type != "flac" {
		t.Fatalf("wrong stream: %+v", stream)
	}
}

func TestSongURLRecoversTransientRiskWithoutLoweringQuality(t *testing.T) {
	var requests []songURLRequest
	songURLServer(t, func(w http.ResponseWriter, r *http.Request) {
		request, err := decodeSongURLRequest(r)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests = append(requests, request)
		if len(requests) == 1 {
			io.WriteString(w, `{"code":-460,"message":"temporary network risk"}`)
			return
		}
		io.WriteString(w, songURLSuccess)
	})
	stream, err := SongURL(42, "hires")
	if err != nil || stream == nil || stream.URL != "https://audio.example/song.flac" || stream.Level != "hires" {
		t.Fatalf("transient failure did not recover full requested stream: stream=%+v err=%v", stream, err)
	}
	if len(requests) != 2 {
		t.Fatalf("recovery requests = %d, want 2", len(requests))
	}
	for _, request := range requests {
		if request.IDs != "[42]" || request.Level != "hires" || request.EncodeType != "flac" {
			t.Fatalf("recovery changed selected track or quality: %+v", request)
		}
	}
}

func TestSongURLDoesNotMisclassifyServiceFailuresAsVIP(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response string
		requests int
		message  string
	}{
		{"persistent risk", `{"code":-460,"message":"network risk"}`, 2, "network risk"},
		{"expired login", `{"code":301,"msg":"login expired"}`, 1, "login expired"},
		{"empty data", `{"code":200,"data":[]}`, 1, ""},
		{"missing URL", `{"code":200,"data":[{"id":42,"code":200,"url":null}]}`, 1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			songURLServer(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				io.WriteString(w, tc.response)
			})
			stream, err := SongURL(42, "hires")
			if stream != nil || err == nil || errors.Is(err, ErrUnavailable) {
				t.Fatalf("service failure misclassified as song permission: stream=%+v err=%v", stream, err)
			}
			if requests != tc.requests {
				t.Fatalf("unbounded or inappropriate recovery: requests=%d, want %d", requests, tc.requests)
			}
			if tc.message != "" && !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("service error detail lost: %v", err)
			}
		})
	}
}

func TestSongURLRejectsTrialAudio(t *testing.T) {
	songURLServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":200,"data":[{"id":42,"code":200,"url":"https://audio.example/preview.mp3","type":"mp3","level":"exhigh","freeTrialInfo":{"start":0,"end":30}}]}`)
	})
	stream, err := SongURL(42, "hires")
	if stream != nil || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("trial was treated as full playback: stream=%+v err=%v", stream, err)
	}
}
