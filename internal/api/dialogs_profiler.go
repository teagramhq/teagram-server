//go:build performance

package api

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

type getDialogsProfileStage struct {
	name     string
	duration time.Duration
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
