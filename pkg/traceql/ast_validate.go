package traceql

import "fmt"

// unsupportedError is returned for traceql features that are not yet supported.
type unsupportedError struct {
	feature string
}

func newUnsupportedError(feature string) unsupportedError {
	return unsupportedError{feature: feature}
}

func (e unsupportedError) Error() string {
	return e.feature + " not yet supported"
}

func (r RootExpr) validate() error {
	return r.Pipeline.validate()
}

func (p Pipeline) validate() error {
	for _, p := range p.Elements {
		err := p.validate()
		if err != nil {
			return err
		}
	}
	return nil
}

func (o GroupOperation) validate() error {
	return newUnsupportedError("by()")

	// todo: once grouping is supported the below validation will apply
	// if !o.Expression.referencesSpan() {
	// 	return fmt.Errorf("grouping field expressions must reference the span: %s", o.String())
	// }

	// return o.Expression.validate()
}

func (o CoalesceOperation) validate() error {
	return newUnsupportedError("coalesce()")
}

func (o ScalarOperation) validate() error {
	if err := o.LHS.validate(); err != nil {
		return err
	}
	if err := o.RHS.validate(); err != nil {
		return err
	}

	lhsT := o.LHS.impliedType()
	rhsT := o.RHS.impliedType()
	if !lhsT.isMatchingOperand(rhsT) {
		return fmt.Errorf("binary operations must operate on the same type: %s", o.String())
	}

	if !o.Op.binaryTypesValid(lhsT, rhsT) {
		return fmt.Errorf("illegal operation for the given types: %s", o.String())
	}

	return nil
}

func (a Aggregate) validate() error {
	if a.e == nil {
		return nil
	}

	if err := a.e.validate(); err != nil {
		return err
	}

	// aggregate field expressions require a type of a number or attribute
	t := a.e.impliedType()
	if t != TypeAttribute && !t.isNumeric() {
		return fmt.Errorf("aggregate field expressions must resolve to a number type: %s", a.String())
	}

	if !a.e.referencesSpan() {
		return fmt.Errorf("aggregate field expressions must reference the span: %s", a.String())
	}

	switch a.op {
	case aggregateCount, aggregateAvg, aggregateMin, aggregateMax, aggregateSum:
	default:
		return newUnsupportedError(fmt.Sprintf("aggregate operation (%v)", a.op))
	}

	return nil
}

func (o SpansetOperation) validate() error {
	// TODO validate operator is a SpanSetOperator
	if err := o.LHS.validate(); err != nil {
		return err
	}
	if err := o.RHS.validate(); err != nil {
		return err
	}

	// supported spanset operations
	switch o.Op {
	case OpSpansetChild, OpSpansetDescendant, OpSpansetSibling:
		return newUnsupportedError(fmt.Sprintf("spanset operation (%v)", o.Op))
	}

	return nil
}

func (f SpansetFilter) validate() error {
	if err := f.Expression.validate(); err != nil {
		return err
	}

	t := f.Expression.impliedType()
	if t != TypeAttribute && t != TypeBoolean {
		return fmt.Errorf("span filter field expressions must resolve to a boolean: %s", f.String())
	}

	return nil
}

func (f ScalarFilter) validate() error {
	if err := f.lhs.validate(); err != nil {
		return err
	}
	if err := f.rhs.validate(); err != nil {
		return err
	}

	lhsT := f.lhs.impliedType()
	rhsT := f.rhs.impliedType()
	if !lhsT.isMatchingOperand(rhsT) {
		return fmt.Errorf("binary operations must operate on the same type: %s", f.String())
	}

	if !f.op.binaryTypesValid(lhsT, rhsT) {
		return fmt.Errorf("illegal operation for the given types: %s", f.String())
	}

	// Only supported expression types
	switch f.lhs.(type) {
	case Aggregate:
	default:
		return newUnsupportedError("scalar filter lhs of type (%v)")
	}

	switch f.rhs.(type) {
	case Static:
	default:
		return newUnsupportedError("scalar filter rhs of type (%v)")
	}

	return nil
}

func (o BinaryOperation) validate() error {
	if err := o.LHS.validate(); err != nil {
		return err
	}
	if err := o.RHS.validate(); err != nil {
		return err
	}

	lhsT := o.LHS.impliedType()
	rhsT := o.RHS.impliedType()

	if !lhsT.isMatchingOperand(rhsT) {
		return fmt.Errorf("binary operations must operate on the same type: %s", o.String())
	}

	if !o.Op.binaryTypesValid(lhsT, rhsT) {
		return fmt.Errorf("illegal operation for the given types: %s", o.String())
	}

	switch o.Op {
	case OpNotRegex,
		OpSpansetChild,
		OpSpansetDescendant,
		OpSpansetSibling:
		return newUnsupportedError(fmt.Sprintf("binary operation (%v)", o.Op))
	}

	return nil
}

func (o UnaryOperation) validate() error {
	if err := o.Expression.validate(); err != nil {
		return err
	}

	t := o.Expression.impliedType()
	if t == TypeAttribute {
		return nil
	}

	if !o.Op.unaryTypesValid(t) {
		return fmt.Errorf("illegal operation for the given type: %s", o.String())
	}

	return nil
}

func (n Static) validate() error {
	if n.Type == TypeNil {
		return newUnsupportedError("nil")
	}

	return nil
}

func (a Attribute) validate() error {
	if a.Parent {
		return newUnsupportedError("parent")
	}
	switch a.Intrinsic {
	case IntrinsicParent,
		IntrinsicChildCount:
		return newUnsupportedError(fmt.Sprintf("intrinsic (%v)", a.Intrinsic))
	}

	return nil
}
