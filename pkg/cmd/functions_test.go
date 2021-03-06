package cmd

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/Knetic/govaluate"
)

func TestFunctions(t *testing.T) {
	testData := []struct {
		x    interface{}
		y    interface{}
		expr string
		res  interface{}
	}{
		{x: 120., y: 100., expr: "ndiff(x,y)", res: .20},
		{x: nil, y: 100., expr: "ndiff(x,y)", res: nil},
		{x: 120., y: nil, expr: "ndiff(x,y)", res: nil},
		{x: nil, y: nil, expr: "ndiff(x,y)", res: nil},
		{x: -120., expr: "abs(x)", res: 120.},
		{x: nil, expr: "abs(x)", res: nil},
		{x: 1.3, expr: "ceil(x)", res: 2.0},
		{x: nil, expr: "ceil(x)", res: nil},
		{x: 1.3, expr: "floor(x)", res: 1.0},
		{x: nil, expr: "floor(x)", res: nil},
		{x: 1.3, expr: "round(x)", res: 1.0},
		{x: 1.6, expr: "round(x)", res: 2.0},
		{x: nil, expr: "round(x)", res: nil},
		{x: 1.0, expr: "log(x)", res: 0.0},
		{x: nil, expr: "log(x)", res: nil},
		{x: 4.0, expr: "sqrt(x)", res: 2.0},
		{x: nil, expr: "sqrt(x)", res: nil},
		{x: []interface{}{2., 3., 1.}, expr: "count(x)", res: 3.},
		{x: []interface{}{2., 3., 1.}, expr: "first(x)", res: 2.},
		{x: []interface{}{2., 3., 1.}, expr: "last(x)", res: 1.},
		{x: []interface{}{1., 2., 3., 4.}, expr: "sum(x)", res: 10.},
		{x: []interface{}{1., 2., 3., 4.}, expr: "avg(x)", res: 2.5},
		{x: []interface{}{1., 2., 3., nil}, expr: "avg(x)", res: 2.},
		{x: []interface{}{1., 1., 2., 3.}, expr: "med(x)", res: 1.5},
		{x: []interface{}{1., 2., 3., 4.}, expr: "min(x)", res: 1.0},
		{x: []interface{}{1., 2., 3., 4.}, expr: "max(x)", res: 4.0},
		{x: []interface{}{}, expr: "med(x)", res: nil},
		{x: []interface{}{}, expr: "count(x)", res: 0.},
		{x: []interface{}{}, expr: "first(x)", res: nil},
		{x: []interface{}{}, expr: "last(x)", res: nil},
		{x: []interface{}{}, expr: "sum(x)", res: nil},
		{x: []interface{}{}, expr: "avg(x)", res: nil},
		{x: []interface{}{}, expr: "med(x)", res: nil},
		{x: []interface{}{}, expr: "min(x)", res: nil},
		{x: []interface{}{}, expr: "max(x)", res: nil},
	}

	for _, test := range testData {
		t.Run(fmt.Sprintf("%v/%v", test.expr, test.x), func(t *testing.T) {
			compiledExp, err := govaluate.NewEvaluableExpressionWithFunctions(test.expr, evalFunctions)
			if err != nil {
				t.Fatal(err)
			}

			vars := map[string]interface{}{
				"x": test.x,
				"y": test.y,
			}
			res, err := compiledExp.Eval(govaluate.MapParameters(vars))
			if err != nil {
				t.Fatal(err)
			}

			if !reflect.DeepEqual(res, test.res) {
				t.Errorf("expected %+v, got %+v", test.res, res)
			}
		})
	}
}

func TestSort(t *testing.T) {
	testData := []struct {
		in  []interface{}
		out []interface{}
	}{
		{
			[]interface{}{3., "foo", nil, 4., "bar"},
			[]interface{}{nil, 3., 4., "bar", "foo"},
		},
	}

	for _, test := range testData {
		t.Run(fmt.Sprintf("%+v", test.in), func(t *testing.T) {
			out := sortArray(test.in)
			if !reflect.DeepEqual(out, test.out) {
				t.Errorf("expected %+v, got %+v", test.out, out)
			}
		})
	}
}
