package cmd

import (
	"math"
	"sort"

	"github.com/Knetic/govaluate"
	"github.com/cockroachdb/errors"
)

func scalarFunction(name string, fn func(float64) (float64, error)) govaluate.ExpressionFunction {
	return func(args ...interface{}) (interface{}, error) {
		if len(args) == 0 {
			return nil, nil
		}
		if len(args) != 1 {
			return nil, errors.Newf("%s: expected 1 argument, got %d", name, len(args))
		}
		if args[0] == nil {
			return nil, nil
		}
		x, ok := args[0].(float64)
		if !ok {
			return nil, errors.Newf("%s: expected scalar, got: %T", name, args[0])
		}
		return fn(x)
	}
}

var evalFunctions = map[string]govaluate.ExpressionFunction{
	// Normalized difference of two scalars.
	// ndiff(x, y) = N  means "the value x differs from y by +/- N%"
	// The second scalar is the reference value.
	"ndiff": func(args ...interface{}) (interface{}, error) {
		if len(args) != 2 {
			return nil, errors.Newf("ndiff: expected 2 argument, got %d", len(args))
		}
		if args[0] == nil || args[1] == nil {
			return nil, nil
		}
		x, ok1 := args[0].(float64)
		y, ok2 := args[1].(float64)
		if !ok1 || !ok2 {
			return nil, errors.Newf("ndiff: expected two scalars, got: %T, %T", args[0], args[1])
		}

		diff := math.Abs(x - y)
		ref := math.Abs(y)
		return diff / ref, nil
	},

	// Absolute value.
	"abs": scalarFunction("abs", func(x float64) (float64, error) { return math.Abs(x), nil }),

	// Ceiling.
	"ceil": scalarFunction("ceil", func(x float64) (float64, error) { return math.Ceil(x), nil }),

	// Floor.
	"floor": scalarFunction("floor", func(x float64) (float64, error) { return math.Floor(x), nil }),

	// Round.
	"round": scalarFunction("round", func(x float64) (float64, error) { return math.Round(x), nil }),

	// Natural logarithm.
	"log": scalarFunction("log", func(x float64) (float64, error) { return math.Log(x), nil }),

	// Square root.
	"sqrt": scalarFunction("sqrt", func(x float64) (float64, error) { return math.Sqrt(x), nil }),

	// Count of array.
	"count": func(args ...interface{}) (interface{}, error) {
		return float64(len(args)), nil
	},

	// Last element in array. nil if array is empty.
	"last": func(args ...interface{}) (interface{}, error) {
		if len(args) == 0 {
			return nil, nil
		}
		return args[len(args)-1], nil
	},

	// First element in array. nil if array is empty.
	"first": func(args ...interface{}) (interface{}, error) {
		if len(args) == 0 {
			return nil, nil
		}
		return args[0], nil
	},

	// Sorted array.
	"sorted": func(args ...interface{}) (interface{}, error) {
		if len(args) == 0 {
			return nil, nil
		}
		return sortArray(args), nil
	},

	// Sum of array. nil if no values.
	"sum": func(args ...interface{}) (interface{}, error) {
		var sum float64
		count := 0
		for _, v := range args {
			if v == nil {
				continue
			}
			switch x := v.(type) {
			case float64:
				sum += x
			case bool:
				if x {
					sum += 1
				}
			default:
				return nil, errors.Newf("avg: unknown value type: %T", v)
			}
			count++
		}
		if count == 0 {
			return nil, nil
		}
		return sum, nil
	},

	// Minimum of array. nil if no element. Only works with scalars/bools.
	"min": func(args ...interface{}) (interface{}, error) {
		min := math.Inf(1)
		count := 0
		for _, v := range args {
			if v == nil {
				continue
			}
			switch x := v.(type) {
			case float64:
				if x < min {
					min = x
				}
			case bool:
				val := 0.0
				if x {
					val = 1.0
				}
				if val < min {
					min = val
				}
			default:
				return nil, errors.Newf("min: unknown value type: %T", v)
			}
			count++
		}
		if count == 0 {
			return nil, nil
		}
		return min, nil
	},

	// Maximum of array. nil if no element. Only works with scalars/bools.
	"max": func(args ...interface{}) (interface{}, error) {
		max := math.Inf(-1)
		count := 0
		for _, v := range args {
			if v == nil {
				continue
			}
			switch x := v.(type) {
			case float64:
				if x > max {
					max = x
				}
			case bool:
				val := 0.0
				if x {
					val = 1.0
				}
				if val > max {
					max = val
				}
			default:
				return nil, errors.Newf("max: unknown value type: %T", v)
			}
			count++
		}
		if count == 0 {
			return nil, nil
		}
		return max, nil
	},

	// Average of array. nil if no element.
	"avg": func(args ...interface{}) (interface{}, error) {
		var sum float64
		count := 0
		for _, v := range args {
			if v == nil {
				continue
			}
			switch x := v.(type) {
			case float64:
				sum += x
			case bool:
				if x {
					sum += 1
				}
			default:
				return nil, errors.Newf("avg: unknown value type: %T", v)
			}
			count++
		}
		if count == 0 {
			return nil, nil
		}
		return sum / float64(count), nil
	},

	// Median of array. nil if no element.
	"med": func(args ...interface{}) (interface{}, error) {
		var vals []float64
		for _, v := range args {
			if v == nil {
				continue
			}
			switch x := v.(type) {
			case float64:
				vals = append(vals, x)
			case bool:
				if x {
					vals = append(vals, 1)
				} else {
					vals = append(vals, 0)
				}
			default:
				return nil, errors.Newf("med: unknown value type: %T", v)
			}
		}
		if len(vals) == 0 {
			return nil, nil
		}
		sort.Float64s(vals)
		if len(vals)%2 == 1 {
			// if len = 5, we pick item at idx 2.
			return vals[(len(vals)-1)/2], nil
		}
		// If len = 6, we avg items at idx 2 and 3.
		x1 := vals[len(vals)/2-1]
		x2 := vals[len(vals)/2]
		return (x1 + x2) / 2, nil
	},
}

func init() {
	// Some handy aliases.
	evalFunctions["normalized_difference"] = evalFunctions["ndiff"]
	evalFunctions["median"] = evalFunctions["med"]
	evalFunctions["average"] = evalFunctions["avg"]
}

type collectFn func(a []interface{}, n int, x interface{}) ([]interface{}, error)

var collectFns = map[assignMode]collectFn{
	assignFirstN: func(a []interface{}, n int, x interface{}) ([]interface{}, error) {
		if x == nil || len(a) >= n {
			return a, nil
		}
		return append(a, x), nil
	},
	assignLastN: func(a []interface{}, n int, x interface{}) ([]interface{}, error) {
		if x == nil {
			return a, nil
		}
		if len(a) >= n {
			a = a[1:]
		}
		return append(a, x), nil
	},
	assignTopN: func(a []interface{}, n int, x interface{}) ([]interface{}, error) {
		if x == nil {
			return a, nil
		}
		var val float64
		switch v := x.(type) {
		case bool:
			if v {
				val = 1
			}
		case float64:
			val = v
		default:
			// Ensure the array is returned, so that it remains unchanged in
			// the caller.
			return a, errors.Newf("top N: expected numeric value, got %T", x)
		}
		res := make([]interface{}, len(a)+1)
		i := 0
		for ; i < len(a) && a[i].(float64) >= val; i++ {
			res[i] = a[i]
		}
		res[i] = val
		if i < len(a) {
			copy(res[i+1:], a[i:])
		}
		if len(res) > n {
			res = res[:n]
		}
		return res, nil
	},
	assignBottomN: func(a []interface{}, n int, x interface{}) ([]interface{}, error) {
		if x == nil {
			return a, nil
		}
		var val float64
		switch v := x.(type) {
		case bool:
			if v {
				val = 1
			}
		case float64:
			val = v
		default:
			// Ensure the array is returned, so that it remains unchanged in
			// the caller.
			return a, errors.Newf("bottom N: expected numeric value, got %T", x)
		}
		res := make([]interface{}, len(a)+1)
		i := 0
		for ; i < len(a) && a[i].(float64) <= val; i++ {
			res[i] = a[i]
		}
		res[i] = val
		if i < len(a) {
			copy(res[i+1:], a[i:])
		}
		if len(res) > n {
			res = res[:n]
		}
		return res, nil
	},
}

func sortArray(a []interface{}) []interface{} {
	s := sortable{a: a}
	sort.Sort(&s)
	return s.a
}

type sortable struct {
	copied bool
	a      []interface{}
}

var _ sort.Interface = (*sortable)(nil)

func (s *sortable) Len() int {
	return len(s.a)
}

func (s *sortable) Swap(i, j int) {
	if !s.copied {
		newA := make([]interface{}, len(s.a))
		copy(newA, s.a)
		s.a = newA
		s.copied = true
	}
	s.a[i], s.a[j] = s.a[j], s.a[i]
}

// ordering: nils, then scalars, then strings, then other things.
// bools are considered scalars.
func (s sortable) Less(i, j int) bool {
	x := s.a[i]
	y := s.a[j]
	if x == nil {
		return y != nil
	}
	if sx, ok := scalarConv(x); ok {
		if y == nil {
			return false
		}
		if sy, ok := scalarConv(y); ok {
			return sx < sy
		}
		// y is a string or other.
		return true
	}
	if sx, ok := x.(string); ok {
		if y == nil {
			return false
		}
		if _, ok := scalarConv(y); ok {
			return false
		}
		if sy, ok := y.(string); ok {
			return sx < sy
		}
		// y is not a scalar.
		return true
	}
	// x is not one of the scalars. Probably an array.
	if y == nil {
		return false
	}
	if _, ok := scalarConv(y); ok {
		return false
	}
	if _, ok := y.(string); ok {
		return false
	}
	return true
}

func scalarConv(x interface{}) (float64, bool) {
	switch t := x.(type) {
	case bool:
		if t {
			return 1, true
		} else {
			return 0, true
		}
	case float64:
		return t, true
	}
	return 0, false
}
