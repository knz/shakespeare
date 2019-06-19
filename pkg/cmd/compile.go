package cmd

import (
	"fmt"
	"io"
	"time"
)

// compile transforms a programmatic description
// of a play (using stanzas) into an explicit list of steps.
func (cfg *config) compile() error {
	// Compute the maximum script length.
	scriptLen := 0
	for _, s := range cfg.stanzas {
		if len(s.script) > scriptLen {
			scriptLen = len(s.script)
		}
	}

	// We know how long the play is going to be.
	cfg.play = make([]scene, scriptLen+1)

	// Compile the script.
	atTime := time.Duration(0)
	for i := 0; i < scriptLen; i++ {
		thisAct := &cfg.play[i]
		thisAct.waitUntil = atTime
		for _, s := range cfg.stanzas {
			script := s.script
			if i >= len(script) {
				continue
			}

			thisAct.concurrentLines = append(thisAct.concurrentLines, scriptLine{actor: s.actor})
			curLine := &thisAct.concurrentLines[len(thisAct.concurrentLines)-1]

			aChar := script[i]
			acts := cfg.actions[aChar]

			for _, act := range acts {
				switch act.typ {
				case ambianceAction:
					curLine.steps = append(curLine.steps,
						step{typ: stepAmbiance, action: act.act})
				case doAction:
					curLine.steps = append(curLine.steps,
						step{typ: stepDo, action: act.act})
				}
			}
			if len(curLine.steps) == 0 {
				// No steps actually generated (or only nops). Erase the last line.
				thisAct.concurrentLines = thisAct.concurrentLines[:len(thisAct.concurrentLines)-1]
			}
		}
		atTime += cfg.tempo
	}
	cfg.play[len(cfg.play)-1].waitUntil = atTime
	return nil
}

// printSteps prints the generated steps.
func (cfg *config) printSteps(w io.Writer) {
	fmt.Fprintln(w, "# play")
	for i, s := range cfg.play {
		if s.waitUntil != 0 && (i == len(cfg.play)-1 || len(s.concurrentLines) > 0) {
			fmt.Fprintf(w, "#  (wait until %s)\n", s.waitUntil)
		}
		comma := ""
		for _, line := range s.concurrentLines {
			if len(line.steps) > 0 {
				fmt.Fprint(w, comma)
				comma = "#  (meanwhile)\n"
			}
			for _, step := range line.steps {
				switch step.typ {
				case stepDo:
					fmt.Fprintf(w, "#  %s: %s!\n", line.actor.name, step.action)
				case stepAmbiance:
					fmt.Fprintf(w, "#  (mood: %s)\n", step.action)
				}
			}
		}
	}
	fmt.Fprintln(w, "# end")
}

// scene describes one scene of the play.
type scene struct {
	waitUntil       time.Duration
	concurrentLines []scriptLine
}

func (s *scene) isEmpty() bool {
	return len(s.concurrentLines) == 0
}

// scriptLine describes what one actor should play during one scene.
type scriptLine struct {
	actor *actor
	steps []step
}

// step is one action for one actor during the scene.
type step struct {
	typ    stepType
	action string
}

type stepType int

const (
	stepDo stepType = iota
	stepAmbiance
)
