package cmd

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
)

type sceneSpec struct {
	name      string
	entails   []actionGroup
	moodStart string
	moodEnd   string
}

type actionGroup struct {
	actor   *actor
	actions []string
}

func (cfg *config) compileV2() error {
	if len(cfg.storyLine) == 0 {
		return nil
	}

	nopScene := &sceneSpec{name: "."}
	cfg.play = nil
	for _, script := range cfg.storyLine {
		actLen := len(script)
		thisAct := make([]scene, 0, actLen+1)
		atTime := time.Duration(0)

		moodStart, moodEnd := "", ""
		thisScene := scene{}

		for scriptIdx := 0; scriptIdx < len(script); scriptIdx++ {
			scChar := script[scriptIdx]
			var sc *sceneSpec
			switch scChar {
			case '.':
				// nop: empty scene
				sc = nopScene
			case '+', '_':
				return errors.AssertionFailedf("unexpected %c at position %d in: %s", scChar, scriptIdx, script)
			default:
				sc = cfg.sceneSpecs[scChar]
			}

			if sc.moodStart != "" && moodStart == "" {
				moodStart = sc.moodStart
			}
			if sc.moodEnd != "" {
				moodEnd = sc.moodEnd
			}

			for _, entail := range sc.entails {
				thisScene.concurrentLines = append(thisScene.concurrentLines, scriptLine{actor: entail.actor})
				curLine := &thisScene.concurrentLines[len(thisScene.concurrentLines)-1]

				for _, act := range entail.actions {
					failOk := false
					if strings.HasSuffix(act, "?") {
						act = strings.TrimSuffix(act, "?")
						failOk = true
					}
					curLine.steps = append(curLine.steps,
						step{typ: stepDo, action: act, failOk: failOk})
				}
				if len(curLine.steps) == 0 {
					// No steps actually generated. Erase the last line.
					thisScene.concurrentLines = thisScene.concurrentLines[:len(thisScene.concurrentLines)-1]
				}
			}

			if scriptIdx < len(script)-1 && script[scriptIdx+1] == '+' {
				// More concurrent actions.
				scriptIdx++
				continue
			}

			// End of scene.
			if moodStart != "" {
				// Inject an empty scene with just a mood change in the act at this time.
				s := scene{
					waitUntil:       atTime,
					concurrentLines: []scriptLine{scriptLine{steps: []step{step{typ: stepAmbiance, action: moodStart}}}},
				}
				thisAct = append(thisAct, s)
				moodStart = ""
			}
			thisScene.waitUntil = atTime
			atTime += cfg.tempo
			if !thisScene.isEmpty() {
				thisAct = append(thisAct, thisScene)
				thisScene = scene{}
			}
			if moodEnd != "" {
				// Inject an empty scene with just a mood change in the act at this time.
				s := scene{
					waitUntil:       0,
					concurrentLines: []scriptLine{scriptLine{steps: []step{step{typ: stepAmbiance, action: moodEnd}}}},
				}
				thisAct = append(thisAct, s)
				moodEnd = ""
			}
		}

		// Wait at end of act for remaining time.
		thisAct = append(thisAct, scene{waitUntil: atTime})
		cfg.play = append(cfg.play, thisAct)
	}
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
		var st string
		if k < len(cfg.storyLine) {
			st = fmt.Sprintf(": %s", cfg.storyLine[k])
		}
		fmt.Fprintf(w, "# -- ACT %d%s --\n", k+1, st)
		atTime := time.Duration(0)
		for i, s := range act {
			if s.waitUntil != 0 {
				if i == len(act)-1 || (s.waitUntil != atTime && !s.isEmpty()) {
					fmt.Fprintf(w, "# %2d:  (wait until %s)\n", i+1, s.waitUntil)
				}
				atTime = s.waitUntil
			}
			comma := ""
			for _, line := range s.concurrentLines {
				if len(line.steps) > 0 {
					fmt.Fprint(w, comma)
					comma = fmt.Sprintf("# %2d:  (meanwhile)\n", i+1)
				}
				for _, step := range line.steps {
					switch step.typ {
					case stepDo:
						actChar := '!'
						if step.failOk {
							actChar = '?'
						}
						fmt.Fprintf(w, "# %2d:  %s: %s%c\n", i+1, fan(line.actor.name), facn(step.action), actChar)
					case stepAmbiance:
						fmt.Fprintf(w, "# %2d:  (%s: %s)\n", i+1, fkw("mood"), step.action)
					}
				}
			}
		}
	}
	if cfg.repeatActNum > 0 {
		fmt.Fprintf(w, "# -- REPEATING FROM ACT %d --\n", cfg.repeatActNum)
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
