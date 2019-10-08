package cmd

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

func GenName(val int64) string {
	rem, w1 := choose(nounsList, float64(val))
	rem, w2 := choose(adjList, rem)
	rem, w3 := choose(advList, rem)
	d := int(rem)
	number := ""
	if d > 0 {
		number = strconv.Itoa(d)
		switch {
		case strings.HasSuffix(number, "1"):
			number += "st"
		case strings.HasSuffix(number, "2"):
			number += "nd"
		case strings.HasSuffix(number, "3"):
			number += "rd"
		default:
			number += "th"
		}
		number += " "
	}
	return strings.Title(fmt.Sprintf(`%s%s %s %s`, number, w3, w2, w1))
}

func choose(list []string, val float64) (float64, string) {
	numWords := float64(len(list))
	// numBits := math.Ceil(math.Log(float64(numWords)) / math.Log(2))
	// fmt.Fprintln(os.Stderr, "considering", int(numWords), "words, approx ", numBits, "bits of entropy")
	idx := math.Mod(val, numWords)
	word := list[int(idx)]
	return val / numWords, word
}
