package cmd

import "strings"

func combineStoryLines(line1, line2 []string) []string {
	res := make([]string, len(line1))
	i := 0
	for ; i < len(line1); i++ {
		if i >= len(line2) {
			res[i] = line1[i]
			continue
		}
		res[i] = combineActs(line1[i], line2[i])
	}
	for ; i < len(line2); i++ {
		res = append(res, line2[i])
	}
	return res
}

func combineActs(act1, act2 string) string {
	var res strings.Builder
	i1 := 0
	i2 := 0
	for i1 < len(act1) {
		var s1, s2 string
		s1, i1 = extractAction(act1, i1)
		s2, i2 = extractAction(act2, i2)
		if s1 == "." {
			if s2 != "" {
				res.WriteString(s2)
			} else {
				res.WriteByte('.')
			}
		} else {
			res.WriteString(s1)
			if s2 != "" && s2 != "." {
				res.WriteByte('+')
				res.WriteString(s2)
			}
		}
	}
	for ; i2 < len(act2); i2++ {
		res.WriteByte(act2[i2])
	}
	return res.String()
}

func extractAction(act1 string, i1 int) (string, int) {
	if i1 >= len(act1) {
		return "", i1
	}
	s1 := []byte{act1[i1]}
	end1 := i1 + 1
	for end1+1 < len(act1) {
		if act1[end1] != '+' {
			break
		}
		s1 = append(s1, '+', act1[end1+1])
		i1 += 2
		end1 += 2
	}
	return string(s1), end1
}
