package main

import (
	"fmt"
	"time"
)

// compile transforms a programmatic description
// of a play (using stanzas) into an explicit list of steps.
func compile() error {
	// Compute the maximum script length.
	scriptLen := 0
	for _, s := range stanzas {
		if len(s.script) > scriptLen {
			scriptLen = len(s.script)
		}
	}

	// We know how long the play is going to be.
	play = make([]act, scriptLen+1)

	// Compile the script.
	atTime := time.Duration(0)
	for i := 0; i < scriptLen; i++ {
		thisAct := &play[i]
		thisAct.waitUntil = atTime
		for _, s := range stanzas {
			script := s.script
			if i >= len(script) {
				continue
			}

			thisAct.concurrentLines = append(thisAct.concurrentLines, scriptLine{actor: s.actor})
			curLine := &thisAct.concurrentLines[len(thisAct.concurrentLines)-1]

			aChar := script[i]
			acts := actions[aChar]

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
		atTime += tempo
	}
	play[len(play)-1].waitUntil = atTime
	return nil
}

// printSteps prints the generated steps.
func printSteps() {
	fmt.Println("")
	fmt.Println("# play")
	for i, s := range play {
		if s.waitUntil != 0 && (i == len(play)-1 || len(s.concurrentLines) > 0) {
			fmt.Printf("#  (wait until %s)\n", s.waitUntil)
		}
		comma := ""
		for _, line := range s.concurrentLines {
			if len(line.steps) > 0 {
				fmt.Print(comma)
				comma = "#  (meanwhile)\n"
			}
			for _, step := range line.steps {
				switch step.typ {
				case stepDo:
					fmt.Printf("#  %s: %s!\n", line.actor.name, step.action)
				case stepAmbiance:
					fmt.Printf("#  (mood: %s)\n", step.action)
				}
			}
		}
	}
	fmt.Println("# end")
}

// act describes one act of the play.
type act struct {
	waitUntil       time.Duration
	concurrentLines []scriptLine
}

// scriptLine describes what one actor should play during one act.
type scriptLine struct {
	actor *actor
	steps []step
}

// step is one action for one actor during the act.
type step struct {
	typ    stepType
	action string
}

type stepType int

const (
	stepDo stepType = iota
	stepAmbiance
)

// play is the list of actions to play.
// This is populated by compile().
var play []act

// FIXME
var steps []oldstep

type oldstep struct {
	typ       stepType
	dur       time.Duration
	character string
	action    string
	targets   []string
}
