//go:build performance

package api

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/gotd/td/tgerr"
)

type getDialogsProfileStage struct {
	name     string
	duration time.Duration
}

func dialogsRateLimitStage(err error) string {
	var rpc *tgerr.Error
	if errors.As(err, &rpc) && rpc.Code == 420 && rpc.Type == "FLOOD_WAIT" {
		return fmt.Sprintf("first-page flood wait %ds", rpc.Argument)
	}
	return "first-page rate limit error"
}

func (p *getDialogsProfiler) rateLimitError(err error) {
	if p != nil {
		p.mark(dialogsRateLimitStage(err))
	}
}

type getDialogsProfiler struct {
	last   time.Time
	stages []getDialogsProfileStage
	emit   func(string, time.Duration)
}

func newGetDialogsProfiler(emit func(string, time.Duration)) *getDialogsProfiler {
	return &getDialogsProfiler{last: time.Now(), emit: emit}
}

func startGetDialogsProfiler() *getDialogsProfiler {
	if os.Getenv("TG_PERF_PROFILE_GETDIALOGS") == "" {
		return nil
	}
	fixture := os.Getenv("TG_PERF_FIXTURE")
	var stages []string
	return newGetDialogsProfiler(func(name string, duration time.Duration) {
		if name == "server handler total" {
			log.Printf("messages.getDialogs server %s stages: %s; total %.2f ms", fixture,
				strings.Join(stages, " "), float64(duration.Microseconds())/1000)
			return
		}
		stages = append(stages, fmt.Sprintf("%s=%.2fms", name, float64(duration.Microseconds())/1000))
	})
}

func (p *getDialogsProfiler) mark(name string) {
	if p == nil {
		return
	}
	now := time.Now()
	p.stages = append(p.stages, getDialogsProfileStage{name: name, duration: now.Sub(p.last)})
	p.last = now
}

func (p *getDialogsProfiler) finish() {
	if p == nil {
		return
	}
	p.mark("handler completion")
	var total time.Duration
	for _, stage := range p.stages {
		p.emit(stage.name, stage.duration)
		total += stage.duration
	}
	p.emit("server handler total", total)
}
