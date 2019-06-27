package cmd

import (
	"fmt"
	"io"
	"time"
)

// compileV1 transforms a programmatic description
// of a play (using stanzas) into an explicit list of steps.
func (cfg *config) compileV1() error {
	// Compute the maximum script length.
	scriptLen := 0
	for _, s := range cfg.stanzas {
		if len(s.script) > scriptLen {
			scriptLen = len(s.script)
		}
	}

	// We know how long the play is going to be.
	play := make([]scene, scriptLen+1)

	// Compile the script.
	atTime := time.Duration(0)
	for i := 0; i < scriptLen; i++ {
		thisScene := &play[i]
		thisScene.waitUntil = atTime
		for _, s := range cfg.stanzas {
			script := s.script
			if i >= len(script) {
				continue
			}

			thisScene.concurrentLines = append(thisScene.concurrentLines, scriptLine{actor: s.actor})
			curLine := &thisScene.concurrentLines[len(thisScene.concurrentLines)-1]

			aChar := script[i]
			acts := cfg.actions[aChar]

			for _, act := range acts {
				switch act.typ {
				case ambianceAction:
					curLine.steps = append(curLine.steps,
						step{typ: stepAmbiance, action: act.act})
				case doAction:
					curLine.steps = append(curLine.steps,
						step{typ: stepDo, action: act.act, failOk: act.failOk})
				}
			}
			if len(curLine.steps) == 0 {
				// No steps actually generated (or only nops). Erase the last line.
				thisScene.concurrentLines = thisScene.concurrentLines[:len(thisScene.concurrentLines)-1]
			}
		}
		atTime += cfg.tempo
	}
	play[len(play)-1].waitUntil = atTime

	cfg.play = [][]scene{play}

	return nil
}

// printSteps prints the generated steps.
func (cfg *config) printSteps(w io.Writer, annot bool) {
	fkw, fan, facn := fid, fid, fid
	if annot {
		fkw = fw("kw")   // keyword
		fan = fw("an")   // actor name
		facn = fw("acn") // action name
	}
	fmt.Fprintln(w, "# play")
	for k, act := range cfg.play {
		fmt.Fprintf(w, "# -- ACT %d --\n", k+1)
		for i, s := range act {
			if s.waitUntil != 0 && (i == len(act)-1 || len(s.concurrentLines) > 0) {
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
						actChar := '!'
						if step.failOk {
							actChar = '?'
						}
						fmt.Fprintf(w, "#  %s: %s%c\n", fan(line.actor.name), facn(step.action), actChar)
					case stepAmbiance:
						fmt.Fprintf(w, "#  (%s: %s)\n", fkw("mood"), step.action)
					}
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
	failOk bool
}

type stepType int

const (
	stepDo stepType = iota
	stepAmbiance
)
