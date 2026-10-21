package gamestream

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

func TestVideoEncodingLogSurvivesRebuildsAndTracksTransitions(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	x := &streamSession{}
	x.logVideoEncoder("", "test-hw")
	x.logVideoEncoder("h264", "test-hw")
	x.logVideoEncoder("h264", "passthrough")
	x.logVideoEncoder("h264", "passthrough")
	x.logVideoEncoder("hevc", "passthrough")
	x.logVideoEncoder("hevc", "test-hw")
	if strings.Count(output.String(), "gamestream: video codec=") != 4 {
		t.Fatalf("rebuild duplicated logs or transition lost: %s", output.String())
	}
}
