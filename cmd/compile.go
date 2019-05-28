package main

import (
	"fmt"
	"time"
)

// compile transforms a programmatic description
// of a play (using stanzas) into an explicit list of steps.
func compile() {
	atTime := time.Duration(0)
	i := 0
	for {
		foundAct := false
		foundDo := false
		for _, s := range stanzas {
			if i < len(s) {
				foundAct = true
				aChar := s[i]
				acts := actions[aChar]
				for _, act := range acts {
					switch act.typ {
					case ambianceAction:
						steps = append(steps,
							step{typ: stepAmbiance, action: act.act})
					case doAction:
						for _, t := range act.targets {
							if !foundDo {
								steps = append(steps,
									step{typ: stepWaitUntil, dur: atTime})
							}

							foundDo = true
							steps = append(steps,
								step{typ: stepDo, character: t, action: act.act})
						}
					}
				}
			}
		}
		if !foundAct {
			break
		}
		atTime += tempo
		i++
	}
	steps = append(steps,
		step{typ: stepWaitUntil, dur: atTime})
}

// printSteps prints the generated steps.
func printSteps() {
	fmt.Println("")
	fmt.Println("# play")
	for _, s := range steps {
		fmt.Printf("#  %s\n", s.String())
	}
	fmt.Println("# end")
}

// step is one action during the play.
type step struct {
	typ       stepType
	dur       time.Duration
	character string
	action    string
}

func (s *step) String() string {
	switch s.typ {
	case stepWaitUntil:
		return fmt.Sprintf("(wait until %s)", s.dur)
	case stepDo:
		return fmt.Sprintf("%s: %s!", s.character, s.action)
	case stepAmbiance:
		return fmt.Sprintf("(mood: %s)", s.action)
	}
	return "<step???>"
}

type stepType int

const (
	stepDo stepType = iota
	stepWaitUntil
	stepAmbiance
)

// steps is the list of actions to play.
// This is populated by compile().
var steps []step
