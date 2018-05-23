package sysmetrics

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ubuntu/ubuntu-report/internal/helper"
	"github.com/ubuntu/ubuntu-report/internal/metrics"
)

var Update = flag.Bool("update", false, "update golden files")

const (
	// ExpectedReportItem is the field we expect to always get in JSON
	ExpectedReportItem = `"Version":`

	// OptOutJSON is the data sent in case of Opt-Out choice
	// export the private field for tests
	OptOutJSON = optOutJSON
)

func TestMetricsCollect(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name             string
		root             string
		caseGPU          string
		caseScreen       string
		casePartition    string
		caseArchitecture string
		env              map[string]string

		// note that only an internal json package error can make it returning an error
		wantErr bool
	}{
		{"regular",
			"testdata/good", "one gpu", "one screen", "one partition", "regular",
			map[string]string{"XDG_CURRENT_DESKTOP": "some:thing", "XDG_SESSION_DESKTOP": "ubuntusession", "XDG_SESSION_TYPE": "x12"},
			false},
	}
	for _, tc := range testCases {
		tc := tc // capture range variable for parallel execution
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := helper.Asserter{T: t}

			m, cancelGPU, cancelScreen, cancelPartition, cancelArchitecture := newTestMetricsWithCommands(t, tc.root,
				tc.caseGPU, tc.caseScreen, tc.casePartition, tc.caseArchitecture, tc.env)
			defer cancelGPU()
			defer cancelScreen()
			defer cancelPartition()
			defer cancelArchitecture()
			b1, err1 := metricsCollect(m)

			want := helper.LoadOrUpdateGolden(t, filepath.Join(tc.root, "gold", "metricscollect"), b1, *Update)
			a.CheckWantedErr(err1, tc.wantErr)
			a.Equal(b1, want)

			// second run should return the same thing (idemnpotence)
			m, cancelGPU, cancelScreen, cancelPartition, cancelArchitecture = newTestMetricsWithCommands(t, tc.root,
				tc.caseGPU, tc.caseScreen, tc.casePartition, tc.caseArchitecture, tc.env)
			defer cancelGPU()
			defer cancelScreen()
			defer cancelPartition()
			defer cancelArchitecture()
			b2, err2 := metricsCollect(m)

			a.CheckWantedErr(err2, tc.wantErr)
			var got1, got2 json.RawMessage
			json.Unmarshal(b1, got1)
			json.Unmarshal(b2, got2)
			a.Equal(got1, got2)
		})
	}
}

func TestMetricsSend(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name            string
		root            string
		data            []byte
		ack             bool
		manualServerURL string

		cacheReportP    string
		pendingReportP  string
		shouldHitServer bool
		sHitHat         string
		wantErr         bool
	}{
		{"send data",
			"testdata/good", []byte(`{ "some-data": true }`), true, "",
			"ubuntu-report/ubuntu.18.04", "", true, "/ubuntu/desktop/18.04", false},
		{"nack send data",
			"testdata/good", []byte(`{ "some-data": true }`), false, "",
			"ubuntu-report/ubuntu.18.04", "", true, "/ubuntu/desktop/18.04", false},
		{"no IDs (mandatory)",
			"testdata/no-ids", []byte(`{ "some-data": true }`), true, "",
			"ubuntu-report", "", false, "", true},
		{"no network",
			"testdata/good", []byte(`{ "some-data": true }`), true, "http://localhost:4299",
			"ubuntu-report", "ubuntu-report/pending", false, "", true},
		{"invalid URL",
			"testdata/good", []byte(`{ "some-data": true }`), true, "http://a b.com/",
			"ubuntu-report", "", false, "", true},
		{"unwritable path",
			"testdata/good", []byte(`{ "some-data": true }`), true, "",
			"/unwritable/cache/path", "", true, "/ubuntu/desktop/18.04", true},
	}
	for _, tc := range testCases {
		tc := tc // capture range variable for parallel execution
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := helper.Asserter{T: t}

			m := metrics.NewTestMetrics(tc.root, nil, nil, nil, nil, os.Getenv)
			out, tearDown := helper.TempDir(t)
			defer tearDown()
			if strings.HasPrefix(tc.cacheReportP, "/") {
				// absolute path, override temporary one
				out = tc.cacheReportP
			}
			serverHitAt := ""
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				serverHitAt = r.URL.String()
			}))
			defer ts.Close()
			url := tc.manualServerURL
			if url == "" {
				url = ts.URL
			}

			err := metricsSend(m, tc.data, tc.ack, false, url, out, os.Stdout, os.Stdin)

			a.CheckWantedErr(err, tc.wantErr)
			// check we didn't do too much work on error
			if err != nil {
				if !tc.shouldHitServer {
					a.Equal(serverHitAt, "")
				}
				if tc.shouldHitServer && serverHitAt == "" {
					t.Error("we should have hit the local server and it didn't")
				}
				if tc.pendingReportP == "" {
					if _, err := os.Stat(filepath.Join(out, tc.cacheReportP)); !os.IsNotExist(err) {
						t.Errorf("we didn't expect finding a cache report path as we erroring out")
					}
				} else {
					gotF, err := os.Open(filepath.Join(out, tc.pendingReportP))
					if err != nil {
						t.Fatal("didn't generate a pending report file on disk", err)
					}
					got, err := ioutil.ReadAll(gotF)
					if err != nil {
						t.Fatal("couldn't read generated pending report file", err)
					}
					want := helper.LoadOrUpdateGolden(t, filepath.Join(tc.root, "gold", fmt.Sprintf("metricssendpending.%s.%t", strings.Replace(tc.name, " ", "_", -1), tc.ack)), got, *Update)
					a.Equal(got, want)
				}
				return
			}
			a.Equal(serverHitAt, tc.sHitHat)
			gotF, err := os.Open(filepath.Join(out, tc.cacheReportP))
			if err != nil {
				t.Fatal("didn't generate a report file on disk", err)
			}
			got, err := ioutil.ReadAll(gotF)
			if err != nil {
				t.Fatal("couldn't read generated report file", err)
			}

			want := helper.LoadOrUpdateGolden(t, filepath.Join(tc.root, "gold", fmt.Sprintf("metricssend.%s.%t", strings.Replace(tc.name, " ", "_", -1), tc.ack)), got, *Update)
			a.Equal(got, want)
		})
	}
}

func TestMultipleMetricsSend(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name         string
		alwaysReport bool

		cacheReportP    string
		shouldHitServer bool
		sHitHat         string
		wantErr         bool
	}{
		{"fail report twice", false, "ubuntu-report/ubuntu.18.04", false, "/ubuntu/desktop/18.04", true},
		{"forcing report twice", true, "ubuntu-report/ubuntu.18.04", true, "/ubuntu/desktop/18.04", false},
	}
	for _, tc := range testCases {
		tc := tc // capture range variable for parallel execution
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := helper.Asserter{T: t}

			m := metrics.NewTestMetrics("testdata/good", nil, nil, nil, nil, os.Getenv)
			out, tearDown := helper.TempDir(t)
			defer tearDown()
			serverHitAt := ""
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				serverHitAt = r.URL.String()
			}))
			defer ts.Close()

			err := metricsSend(m, []byte(`{ "some-data": true }`), true, tc.alwaysReport, ts.URL, out, os.Stdout, os.Stdin)
			if err != nil {
				t.Fatal("Didn't expect first call to fail")
			}

			// second call, reset server
			serverHitAt = ""
			m = metrics.NewTestMetrics("testdata/good", nil, nil, nil, nil, os.Getenv)
			err = metricsSend(m, []byte(`{ "some-data": true }`), true, tc.alwaysReport, ts.URL, out, os.Stdout, os.Stdin)

			a.CheckWantedErr(err, tc.wantErr)
			// check we didn't do too much work on error
			if err != nil {
				if !tc.shouldHitServer {
					a.Equal(serverHitAt, "")
				}
				if tc.shouldHitServer && serverHitAt == "" {
					t.Error("we should have hit the local server and we didn't")
				}
				return
			}
			a.Equal(serverHitAt, tc.sHitHat)
			gotF, err := os.Open(filepath.Join(out, tc.cacheReportP))
			if err != nil {
				t.Fatal("didn't generate a report file on disk", err)
			}
			got, err := ioutil.ReadAll(gotF)
			if err != nil {
				t.Fatal("couldn't read generated report file", err)
			}
			want := helper.LoadOrUpdateGolden(t, filepath.Join("testdata/good", "gold", fmt.Sprintf("metricssend_twice.%s", strings.Replace(tc.name, " ", "_", -1))), got, *Update)
			a.Equal(got, want)
		})
	}
}

func TestMetricsCollectAndSend(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name             string
		root             string
		caseGPU          string
		caseScreen       string
		casePartition    string
		caseArchitecture string
		env              map[string]string
		r                ReportType
		manualServerURL  string

		cacheReportP    string
		pendingReportP  string
		shouldHitServer bool
		sHitHat         string
		wantErr         bool
	}{
		{"regular report auto",
			"testdata/good", "one gpu", "one screen", "one partition", "regular",
			map[string]string{"XDG_CURRENT_DESKTOP": "some:thing", "XDG_SESSION_DESKTOP": "ubuntusession", "XDG_SESSION_TYPE": "x12", "LANG": "fr_FR.UTF-8", "LANGUAGE": "fr_FR.UTF-8"},
			ReportAuto, "",
			"ubuntu-report/ubuntu.18.04", "", true, "/ubuntu/desktop/18.04", false},
		{"regular report OptOut",
			"testdata/good", "one gpu", "one screen", "one partition", "regular",
			map[string]string{"XDG_CURRENT_DESKTOP": "some:thing", "XDG_SESSION_DESKTOP": "ubuntusession", "XDG_SESSION_TYPE": "x12", "LANG": "fr_FR.UTF-8", "LANGUAGE": "fr_FR.UTF-8"},
			ReportOptOut, "",
			"ubuntu-report/ubuntu.18.04", "", true, "/ubuntu/desktop/18.04", false},
		{"no network",
			"testdata/good", "", "", "", "", nil, ReportAuto, "http://localhost:4299", "ubuntu-report", "ubuntu-report/pending", false, "", true},
		{"No IDs (mandatory)",
			"testdata/no-ids", "", "", "", "", nil, ReportAuto, "", "ubuntu-report", "", false, "", true},
		{"Invalid URL",
			"testdata/good", "", "", "", "", nil, ReportAuto, "http://a b.com/", "ubuntu-report", "", false, "", true},
		{"Unwritable path",
			"testdata/good", "", "", "", "", nil, ReportAuto, "", "/unwritable/cache/path", "", true, "/ubuntu/desktop/18.04", true},
	}
	for _, tc := range testCases {
		tc := tc // capture range variable for parallel execution
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := helper.Asserter{T: t}

			m, cancelGPU, cancelScreen, cancelPartition, cancelArchitecture := newTestMetricsWithCommands(t, tc.root,
				tc.caseGPU, tc.caseScreen, tc.casePartition, tc.caseArchitecture, tc.env)
			defer cancelGPU()
			defer cancelScreen()
			defer cancelPartition()
			defer cancelArchitecture()
			out, tearDown := helper.TempDir(t)
			defer tearDown()
			if strings.HasPrefix(tc.cacheReportP, "/") {
				// absolute path, override temporary one
				out = tc.cacheReportP
			}
			serverHitAt := ""
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				serverHitAt = r.URL.String()
			}))
			defer ts.Close()
			url := tc.manualServerURL
			if url == "" {
				url = ts.URL
			}

			err := metricsCollectAndSend(m, tc.r, false, url, out, os.Stdout, os.Stdin)

			a.CheckWantedErr(err, tc.wantErr)
			// check we didn't do too much work on error
			if err != nil {
				if !tc.shouldHitServer {
					a.Equal(serverHitAt, "")
				}
				if tc.shouldHitServer && serverHitAt == "" {
					t.Error("we should have hit the local server and it didn't")
				}
				if tc.pendingReportP == "" {
					if _, err := os.Stat(filepath.Join(out, tc.cacheReportP)); !os.IsNotExist(err) {
						t.Errorf("we didn't expect finding a cache report path as we erroring out")
					}
				} else {
					gotF, err := os.Open(filepath.Join(out, tc.pendingReportP))
					if err != nil {
						t.Fatal("didn't generate a pending report file on disk", err)
					}
					got, err := ioutil.ReadAll(gotF)
					if err != nil {
						t.Fatal("couldn't read generated pending report file", err)
					}
					want := helper.LoadOrUpdateGolden(t, filepath.Join(tc.root, "gold", fmt.Sprintf("pendingreport.ReportType%d", int(tc.r))), got, *Update)
					a.Equal(got, want)
				}
				return
			}
			a.Equal(serverHitAt, tc.sHitHat)
			gotF, err := os.Open(filepath.Join(out, tc.cacheReportP))
			if err != nil {
				t.Fatal("didn't generate a report file on disk", err)
			}
			got, err := ioutil.ReadAll(gotF)
			if err != nil {
				t.Fatal("couldn't read generated report file", err)
			}
			want := helper.LoadOrUpdateGolden(t, filepath.Join(tc.root, "gold", fmt.Sprintf("cachereport.ReportType%d", int(tc.r))), got, *Update)
			a.Equal(got, want)
		})
	}
}

func TestMultipleMetricsCollectAndSend(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name         string
		alwaysReport bool

		cacheReportP    string
		shouldHitServer bool
		sHitHat         string
		wantErr         bool
	}{
		{"fail report twice", false, "ubuntu-report/ubuntu.18.04", false, "/ubuntu/desktop/18.04", true},
		{"forcing report twice", true, "ubuntu-report/ubuntu.18.04", true, "/ubuntu/desktop/18.04", false},
	}
	for _, tc := range testCases {
		tc := tc // capture range variable for parallel execution
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := helper.Asserter{T: t}

			m, cancelGPU, cancelScreen, cancelPartition, cancelArchitecture := newTestMetricsWithCommands(t, "testdata/good",
				"one gpu", "one screen", "one partition", "regular",
				map[string]string{"XDG_CURRENT_DESKTOP": "some:thing", "XDG_SESSION_DESKTOP": "ubuntusession", "XDG_SESSION_TYPE": "x12", "LANG": "fr_FR.UTF-8", "LANGUAGE": "fr_FR.UTF-8"})
			defer cancelGPU()
			defer cancelScreen()
			defer cancelPartition()
			defer cancelArchitecture()
			out, tearDown := helper.TempDir(t)
			defer tearDown()
			serverHitAt := ""
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				serverHitAt = r.URL.String()
			}))
			defer ts.Close()

			err := metricsCollectAndSend(m, ReportAuto, tc.alwaysReport, ts.URL, out, os.Stdout, os.Stdin)
			if err != nil {
				t.Fatal("Didn't expect first call to fail")
			}

			// second call, reset server
			serverHitAt = ""
			m, cancelGPU, cancelScreen, cancelPartition, cancelArchitecture = newTestMetricsWithCommands(t, "testdata/good",
				"one gpu", "one screen", "one partition", "regular",
				map[string]string{"XDG_CURRENT_DESKTOP": "some:thing", "XDG_SESSION_DESKTOP": "ubuntusession", "XDG_SESSION_TYPE": "x12", "LANG": "fr_FR.UTF-8", "LANGUAGE": "fr_FR.UTF-8"})
			defer cancelGPU()
			defer cancelScreen()
			defer cancelPartition()
			defer cancelArchitecture()
			err = metricsCollectAndSend(m, ReportAuto, tc.alwaysReport, ts.URL, out, os.Stdout, os.Stdin)

			a.CheckWantedErr(err, tc.wantErr)
			// check we didn't do too much work on error
			if err != nil {
				if !tc.shouldHitServer {
					a.Equal(serverHitAt, "")
				}
				if tc.shouldHitServer && serverHitAt == "" {
					t.Error("we should have hit the local server and we didn't")
				}
				return
			}
			a.Equal(serverHitAt, tc.sHitHat)
			gotF, err := os.Open(filepath.Join(out, tc.cacheReportP))
			if err != nil {
				t.Fatal("didn't generate a report file on disk", err)
			}
			got, err := ioutil.ReadAll(gotF)
			if err != nil {
				t.Fatal("couldn't read generated report file", err)
			}
			want := helper.LoadOrUpdateGolden(t, filepath.Join("testdata/good", "gold", fmt.Sprintf("cachereport-twice.ReportType%d", int(ReportAuto))), got, *Update)
			a.Equal(got, want)
		})
	}
}

func TestInteractiveMetricsCollectAndSend(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name    string
		answers []string

		cacheReportP       string
		wantWriteAndUpload bool
	}{
		{"yes", []string{"yes"}, "ubuntu-report/ubuntu.18.04", true},
		{"y", []string{"y"}, "ubuntu-report/ubuntu.18.04", true},
		{"YES", []string{"YES"}, "ubuntu-report/ubuntu.18.04", true},
		{"Y", []string{"Y"}, "ubuntu-report/ubuntu.18.04", true},
		{"no", []string{"no"}, "ubuntu-report/ubuntu.18.04", true},
		{"n", []string{"n"}, "ubuntu-report/ubuntu.18.04", true},
		{"NO", []string{"NO"}, "ubuntu-report/ubuntu.18.04", true},
		{"n", []string{"N"}, "ubuntu-report/ubuntu.18.04", true},
		{"quit", []string{"quit"}, "ubuntu-report/ubuntu.18.04", false},
		{"q", []string{"q"}, "ubuntu-report/ubuntu.18.04", false},
		{"QUIT", []string{"QUIT"}, "ubuntu-report/ubuntu.18.04", false},
		{"Q", []string{"Q"}, "ubuntu-report/ubuntu.18.04", false},
		{"default-quit", []string{""}, "ubuntu-report/ubuntu.18.04", false},
		{"garbage-then-quit", []string{"garbage", "yesgarbage", "nogarbage", "quitgarbage", "Q"}, "ubuntu-report/ubuntu.18.04", false},
		{"ctrl-c-input", []string{"CTRL-C"}, "ubuntu-report/ubuntu.18.04", false},
	}
	for _, tc := range testCases {
		tc := tc // capture range variable for parallel execution
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := helper.Asserter{T: t}

			m, cancelGPU, cancelScreen, cancelPartition, cancelArchitecture := newTestMetricsWithCommands(t, "testdata/good",
				"one gpu", "one screen", "one partition", "regular",
				map[string]string{"XDG_CURRENT_DESKTOP": "some:thing", "XDG_SESSION_DESKTOP": "ubuntusession", "XDG_SESSION_TYPE": "x12", "LANG": "fr_FR.UTF-8", "LANGUAGE": "fr_FR.UTF-8"})
			defer cancelGPU()
			defer cancelScreen()
			defer cancelPartition()
			defer cancelArchitecture()
			out, tearDown := helper.TempDir(t)
			defer tearDown()
			serverHitAt := ""
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				serverHitAt = r.URL.String()
			}))
			defer ts.Close()

			stdin, stdinW := io.Pipe()
			stdout, stdoutW := io.Pipe()

			cmdErrs := helper.RunFunctionWithTimeout(t, func() error { return metricsCollectAndSend(m, ReportInteractive, false, ts.URL, out, stdin, stdoutW) })

			gotJSONReport := false
			answerIndex := 0
			scanner := bufio.NewScanner(stdout)
			scanner.Split(ScanLinesOrQuestion)
			for scanner.Scan() {
				txt := scanner.Text()
				// first, we should have a known element
				if strings.Contains(txt, ExpectedReportItem) {
					gotJSONReport = true
				}
				if !strings.Contains(txt, "Do you agree to report this?") {
					continue
				}
				a := tc.answers[answerIndex]
				if a == "CTRL-C" {
					stdinW.Close()
					break
				} else {
					stdinW.Write([]byte(tc.answers[answerIndex] + "\n"))
				}
				answerIndex = answerIndex + 1
				// all answers have be provided
				if answerIndex >= len(tc.answers) {
					stdinW.Close()
					break
				}
			}

			if err := <-cmdErrs; err != nil {
				t.Fatal("didn't expect to get an error, got:", err)
			}
			a.Equal(gotJSONReport, true)

			// check we didn't do too much work on error
			if !tc.wantWriteAndUpload {
				a.Equal(serverHitAt, "")
				if _, err := os.Stat(filepath.Join(out, tc.cacheReportP)); !os.IsNotExist(err) {
					t.Errorf("we didn't expect finding a cache report path as we said to quit")
				}
				return
			}
			if serverHitAt == "" {
				t.Error("we should have hit the local server and we didn't")
			}
			gotF, err := os.Open(filepath.Join(out, tc.cacheReportP))
			if err != nil {
				t.Fatal("didn't generate a report file on disk", err)
			}
			got, err := ioutil.ReadAll(gotF)
			if err != nil {
				t.Fatal("couldn't read generated report file", err)
			}
			want := helper.LoadOrUpdateGolden(t, filepath.Join("testdata/good", "gold", fmt.Sprintf("cachereport-twice.ReportType%d-%s", int(ReportInteractive), strings.Replace(tc.name, " ", "-", -1))), got, *Update)
			a.Equal(got, want)
		})
	}
}

func newMockShortCmd(t *testing.T, s ...string) (*exec.Cmd, context.CancelFunc) {
	t.Helper()
	return helper.ShortProcess(t, "TestMetricsHelperProcess", s...)
}

func newTestMetricsWithCommands(t *testing.T, root, caseGPU, caseScreen, casePartition, caseArch string, env map[string]string) (m metrics.Metrics,
	cancelGPU, cancelSreen, cancelPartition, cancelArchitecture context.CancelFunc) {
	t.Helper()
	cmdGPU, cancelGPU := newMockShortCmd(t, "lspci", "-n", caseGPU)
	cmdScreen, cancelScreen := newMockShortCmd(t, "xrandr", caseScreen)
	cmdPartition, cancelPartition := newMockShortCmd(t, "df", casePartition)
	cmdArchitecture, cancelArchitecture := newMockShortCmd(t, "dpkg", "--print-architecture", caseArch)
	return metrics.NewTestMetrics(root, cmdGPU, cmdScreen, cmdPartition, cmdArchitecture, helper.GetenvFromMap(env)),
		cancelGPU, cancelScreen, cancelPartition, cancelArchitecture
}

// ScanLinesOrQuestion is copy of ScanLines, adding the expected question string as we don't return here
func ScanLinesOrQuestion(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		// We have a full newline-terminated line.
		return i + 1, dropCR(data[0:i]), nil
	}
	if i := bytes.IndexByte(data, ']'); i >= 0 {
		// We have a full newline-terminated line.
		return i + 1, dropCR(data[0:i]), nil
	}
	// If we're at EOF, we have a final, non-terminated line. Return it.
	if atEOF {
		return len(data), dropCR(data), nil
	}
	// Request more data.
	return 0, nil, nil
}

// dropCR drops a terminal \r from the data.
func dropCR(data []byte) []byte {
	if len(data) > 0 && data[len(data)-1] == '\r' {
		return data[0 : len(data)-1]
	}
	return data
}
