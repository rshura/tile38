package server

import (
	"math"
	"strings"

	"github.com/tidwall/geojson"
	"github.com/tidwall/geojson/geometry"
)

// BinaryOp represents various operators for expressions
type BinaryOp byte

// expression operator enum
const (
	NOOP BinaryOp = iota
	AND
	OR
	tokenAND    = "and"
	tokenOR     = "or"
	tokenNOT    = "not"
	tokenLParen = "("
	tokenRParen = ")"
)

// AreaExpression is (maybe negated) either an spatial object or operator +
// children (other expressions).
type AreaExpression struct {
	negate   bool
	obj      geojson.Object
	op       BinaryOp
	children children
}

type children []*AreaExpression

// String representation, helpful in logging.
func (e *AreaExpression) String() (res string) {
	if e.obj != nil {
		res = e.obj.String()
	} else {
		var chStrings []string
		for _, c := range e.children {
			chStrings = append(chStrings, c.String())
		}
		switch e.op {
		case NOOP:
			res = "empty operator"
		case AND:
			res = "(" + strings.Join(chStrings, " "+tokenAND+" ") + ")"
		case OR:
			res = "(" + strings.Join(chStrings, " "+tokenOR+" ") + ")"
		default:
			res = "unknown operator"
		}
	}
	if e.negate {
		res = tokenNOT + " " + res
	}
	return
}

// Return union of rects for all involved objects
func (e *AreaExpression) Rect() (rect geometry.Rect) {
	if e.obj != nil {
		rect = e.obj.Rect()
		return
	}
	var found bool
	for _, c := range e.children {
		childRect := c.Rect()
		if !found {
			rect = childRect
			found = true
		} else {
			rect = geometry.Rect{
				Min: geometry.Point{
					X: math.Min(rect.Min.X, childRect.Min.X), Y: math.Min(rect.Min.Y, childRect.Min.Y)},
				Max: geometry.Point{
					X: math.Max(rect.Max.X, childRect.Max.X), Y: math.Max(rect.Max.Y, childRect.Max.Y)},
			}
		}
	}
	return
}

// Return boolean value modulo negate field of the expression.
func (e *AreaExpression) maybeNegate(val bool) bool {
	if e.negate {
		return !val
	}
	return val
}

// Methods for testing an AreaExpression against the spatial object.
func (e *AreaExpression) testObject(
	o geojson.Object,
	objObjTest func(o1, o2 geojson.Object) bool,
	exprObjTest func(ae *AreaExpression, ob geojson.Object) bool,
) bool {
	if e.obj != nil {
		return objObjTest(e.obj, o)
	}
	switch e.op {
	case AND:
		for _, c := range e.children {
			if !exprObjTest(c, o) {
				return false
			}
		}
		return true
	case OR:
		for _, c := range e.children {
			if exprObjTest(c, o) {
				return true
			}
		}
		return false
	}
	return false
}

func (e *AreaExpression) rawIntersects(o geojson.Object) bool {
	return e.testObject(o, geojson.Object.Intersects, (*AreaExpression).Intersects)
}

func (e *AreaExpression) rawContains(o geojson.Object) bool {
	return e.testObject(o, geojson.Object.Contains, (*AreaExpression).Contains)
}

func (e *AreaExpression) rawWithin(o geojson.Object) bool {
	return e.testObject(o, geojson.Object.Within, (*AreaExpression).Within)
}

func (e *AreaExpression) Intersects(o geojson.Object) bool {
	return e.maybeNegate(e.rawIntersects(o))
}

func (e *AreaExpression) Contains(o geojson.Object) bool {
	return e.maybeNegate(e.rawContains(o))
}

func (e *AreaExpression) Within(o geojson.Object) bool {
	return e.maybeNegate(e.rawWithin(o))
}

// Methods for testing an AreaExpression against another AreaExpression.
func (e *AreaExpression) testExpression(
	other *AreaExpression,
	exprObjTest func(ae *AreaExpression, ob geojson.Object) bool,
	rawExprExprTest func(ae1, ae2 *AreaExpression) bool,
	exprExprTest func(ae1, ae2 *AreaExpression) bool,
) bool {
	if other.negate {
		oppositeExp := &AreaExpression{negate: !e.negate, obj: e.obj, op: e.op, children: e.children}
		nonNegateOther := &AreaExpression{obj: other.obj, op: other.op, children: other.children}
		return exprExprTest(oppositeExp, nonNegateOther)
	}
	if other.obj != nil {
		return exprObjTest(e, other.obj)
	}
	switch other.op {
	case AND:
		for _, c := range other.children {
			if !rawExprExprTest(e, c) {
				return false
			}
		}
		return true
	case OR:
		for _, c := range other.children {
			if rawExprExprTest(e, c) {
				return true
			}
		}
		return false
	}
	return false
}

func (e *AreaExpression) rawIntersectsExpr(other *AreaExpression) bool {
	return e.testExpression(
		other,
		(*AreaExpression).rawIntersects,
		(*AreaExpression).rawIntersectsExpr,
		(*AreaExpression).IntersectsExpr)
}

func (e *AreaExpression) rawWithinExpr(other *AreaExpression) bool {
	return e.testExpression(
		other,
		(*AreaExpression).rawWithin,
		(*AreaExpression).rawWithinExpr,
		(*AreaExpression).WithinExpr)
}

func (e *AreaExpression) rawContainsExpr(other *AreaExpression) bool {
	return e.testExpression(
		other,
		(*AreaExpression).rawContains,
		(*AreaExpression).rawContainsExpr,
		(*AreaExpression).ContainsExpr)
}

func (e *AreaExpression) IntersectsExpr(other *AreaExpression) bool {
	return e.maybeNegate(e.rawIntersectsExpr(other))
}

func (e *AreaExpression) WithinExpr(other *AreaExpression) bool {
	return e.maybeNegate(e.rawWithinExpr(other))
}

func (e *AreaExpression) ContainsExpr(other *AreaExpression) bool {
	return e.maybeNegate(e.rawContainsExpr(other))
}
