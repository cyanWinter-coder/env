package env

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
)

var (
	// ErrPlanClosed is returned when a plan or planner is used after Close.
	// The target struct is never modified in that case.
	ErrPlanClosed = errors.New("env: plan is closed")

	// ErrStalePlan is returned when a committed plan is older than the last
	// generation already committed through the same Planner. The target keeps
	// the values of the newer generation.
	ErrStalePlan = errors.New("env: plan generation is older than the committed generation")

	// ErrInvalidPlan is returned when committing a plan through a Planner
	// that did not create it.
	ErrInvalidPlan = errors.New("env: plan does not belong to this planner")
)

// FieldChange describes what a plan resolved for one struct field.
//
// Values are never exposed here: Old and New are redacted summaries suitable
// for logging. Variables whose names look sensitive (password, token, secret,
// key, ...) are always masked; Options.RedactFunc can extend that set.
type FieldChange struct {
	// Path is the Go field path relative to the parsed struct, e.g.
	// "Server.Port" or "Items[0].Name".
	Path string

	// Key is the environment variable the field resolves from.
	Key string

	// IsDefault reports whether the value came from envDefault instead of
	// from the environment itself.
	IsDefault bool

	// Assigned reports whether the plan would write this field.
	Assigned bool

	// Overwritten reports whether the plan replaces a value that was already
	// non-zero in the target struct.
	Overwritten bool

	// Changed reports whether applying the plan would change the field.
	Changed bool

	// Old and New are redacted summaries of the value before planning and of
	// the planned value.
	Old string
	New string

	// Err is the validation error for this field, if any. It is redacted but
	// still unwraps to the original error.
	Err error
}

// String renders the change in a log-friendly, redacted form.
func (c FieldChange) String() string {
	var sb strings.Builder

	sb.WriteString(c.Path)
	if c.Key != "" {
		fmt.Fprintf(&sb, " (%s)", c.Key)
	}
	if c.IsDefault {
		sb.WriteString(" [default]")
	}
	if c.Err != nil {
		fmt.Fprintf(&sb, " error: %v", c.Err)
		return sb.String()
	}
	if c.Assigned && c.Old != c.New {
		fmt.Fprintf(&sb, ": %s -> %s", c.Old, c.New)
		if c.Overwritten {
			sb.WriteString(" (overrides existing value)")
		}
		return sb.String()
	}
	fmt.Fprintf(&sb, " = %s (unchanged)", c.New)
	return sb.String()
}

// ParsePlan is the result of the planning phase of a two-phase parse: it
// holds everything Parse would have loaded, resolved against the destination
// struct without modifying it.
//
// Call Apply (or Commit) to write it. A plan with errors refuses to apply,
// and a plan from a closed Planner can never be applied.
type ParsePlan struct {
	planner    *Planner
	generation uint64

	target reflect.Value
	next   reflect.Value
	fields []FieldChange
	err    error

	calls  []onSetCall
	unsets []string
	onSet  OnSetFn

	closed  atomic.Bool
	applied atomic.Bool
	mu      sync.Mutex
}

type onSetCall struct {
	key       string
	value     string
	isDefault bool
}

// Generation returns the generation assigned to this plan. Plans created
// through a Planner are ordered by this value; standalone plans report 0.
func (p *ParsePlan) Generation() uint64 {
	return p.generation
}

// Fields returns every environment-bound field the plan inspected, in
// declaration order. The returned slice is a copy.
func (p *ParsePlan) Fields() []FieldChange {
	out := make([]FieldChange, len(p.fields))
	copy(out, p.fields)
	return out
}

// Changes returns the fields that would change the destination struct.
func (p *ParsePlan) Changes() []FieldChange {
	var out []FieldChange
	for _, f := range p.fields {
		if f.Changed {
			out = append(out, f)
		}
	}
	return out
}

// ChangesMap returns the changed fields keyed by their Go field path.
func (p *ParsePlan) ChangesMap() map[string]FieldChange {
	out := make(map[string]FieldChange, len(p.fields))
	for _, f := range p.fields {
		if f.Changed {
			out[f.Path] = f
		}
	}
	return out
}

// Err returns the redacted planning diagnostics, or nil if every field
// validated. The returned error unwraps to the original errors, so
// errors.Is and errors.As keep working.
func (p *ParsePlan) Err() error {
	if p.err == nil {
		return nil
	}
	return &PlanError{Plan: p, Err: p.err}
}

// HasErrors reports whether planning found any error.
func (p *ParsePlan) HasErrors() bool {
	return p.err != nil
}

// Applied reports whether this plan has been written to the target.
func (p *ParsePlan) Applied() bool {
	return p.applied.Load()
}

// Close discards the plan. After Close, Apply returns ErrPlanClosed and the
// target struct is left untouched.
func (p *ParsePlan) Close() {
	p.closed.Store(true)
}

// String renders the plan as a redacted, line-oriented summary.
func (p *ParsePlan) String() string {
	var assigned, changed int
	for _, f := range p.fields {
		if f.Assigned {
			assigned++
		}
		if f.Changed {
			changed++
		}
	}

	var sb strings.Builder
	fmt.Fprintf(
		&sb,
		"env: plan generation=%d fields=%d assigned=%d changed=%d",
		p.generation, len(p.fields), assigned, changed,
	)
	for _, f := range p.fields {
		sb.WriteString("\n  ")
		sb.WriteString(f.String())
	}
	return sb.String()
}

// Apply writes the plan to the target struct. The target either keeps all of
// its previous values or receives all values of this plan.
//
// Apply returns an error and leaves the target untouched when the plan has
// errors, was closed, is stale, or belongs to a closed Planner.
func (p *ParsePlan) Apply() error {
	return p.Commit()
}

// Commit is an alias for Apply.
func (p *ParsePlan) Commit() error {
	if p.planner != nil {
		return p.planner.Commit(p)
	}

	p.mu.Lock()
	if p.closed.Load() {
		p.mu.Unlock()
		return ErrPlanClosed
	}
	if p.err != nil {
		p.mu.Unlock()
		return p.Err()
	}
	if p.applied.Load() {
		p.mu.Unlock()
		return nil
	}
	if err := applyClone(p.target, p.next); err != nil {
		p.mu.Unlock()
		return err
	}
	p.applied.Store(true)
	p.mu.Unlock()

	p.runSideEffects()
	return nil
}

// applyClone copies next into target as a single assignment, converting a
// reflection panic into an error. On error the target is left untouched.
func applyClone(target, next reflect.Value) (err error) {
	if !target.CanSet() || target.Type() != next.Type() {
		return newAggregateError(NotStructPtrError{})
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			err = &ApplyError{Err: fmt.Errorf("env: failed to apply plan: %v", recovered)}
		}
	}()

	target.Set(next)
	return nil
}

// runSideEffects delivers OnSet callbacks and unsets environment variables
// only after the values have been committed.
func (p *ParsePlan) runSideEffects() {
	for _, call := range p.calls {
		if p.onSet != nil {
			p.onSet(call.key, call.value, call.isDefault)
		}
	}
	for _, key := range p.unsets {
		os.Unsetenv(key)
	}
}

// Planner plans and commits configuration reloads for a single destination
// struct.
//
// Planning never writes the destination and may run concurrently: each call
// parses a private copy. Plans receive increasing generations, and Commit
// refuses plans older than the generation already committed, so a slow
// reload can never overwrite a newer one. After Close no commit can modify
// the destination anymore.
type Planner struct {
	target interface{}
	opts   Options

	mu        sync.Mutex
	nextGen   uint64
	committed uint64
	closed    bool
}

// NewPlanner creates a Planner for the given destination struct pointer.
func NewPlanner(v interface{}) *Planner {
	return NewPlannerWithOptions(v, Options{})
}

// NewPlannerWithOptions creates a Planner for the given destination struct
// pointer using the given options.
func NewPlannerWithOptions(v interface{}, opts Options) *Planner {
	return &Planner{target: v, opts: opts}
}

// Plan validates the current environment into a private copy of the target
// struct without modifying it. The plan is always returned when the target
// is a struct pointer; validation failures are reported by the error (which
// is plan.Err()) and by plan.Err() itself.
func (p *Planner) Plan() (*ParsePlan, error) {
	return p.PlanWithOptions(p.opts)
}

// PlanWithOptions is like Plan but uses the given options.
func (p *Planner) PlanWithOptions(opts Options) (*ParsePlan, error) {
	return p.plan(opts)
}

// Reload plans and, only if planning succeeded, commits in one step. It is
// the two-phase equivalent of Parse for service configuration reloads.
func (p *Planner) Reload() error {
	plan, err := p.Plan()
	if err != nil {
		return err
	}
	return p.Commit(plan)
}

// Commit applies a plan created by this Planner. Plans commit in generation
// order: committing an older plan after a newer one fails with ErrStalePlan
// and leaves the target on the newer values.
func (p *Planner) Commit(plan *ParsePlan) error {
	if plan == nil {
		return ErrInvalidPlan
	}

	p.mu.Lock()
	if p.closed || plan.closed.Load() {
		p.mu.Unlock()
		return ErrPlanClosed
	}
	if plan.planner != p {
		p.mu.Unlock()
		return ErrInvalidPlan
	}
	if plan.err != nil {
		p.mu.Unlock()
		return plan.Err()
	}
	if plan.applied.Load() {
		p.mu.Unlock()
		return nil
	}
	if plan.generation <= p.committed {
		committed := p.committed
		p.mu.Unlock()
		return &StalePlanError{
			Generation: plan.generation,
			Committed:  committed,
		}
	}

	if err := applyClone(plan.target, plan.next); err != nil {
		p.mu.Unlock()
		return err
	}
	plan.applied.Store(true)
	p.committed = plan.generation
	p.mu.Unlock()

	plan.runSideEffects()
	return nil
}

// Apply is an alias for Commit.
func (p *Planner) Apply(plan *ParsePlan) error {
	return p.Commit(plan)
}

// Generation returns the last committed generation.
func (p *Planner) Generation() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.committed
}

// Close prevents any further commit through this Planner. The destination
// struct is never written again, including by plans created before Close.
func (p *Planner) Close() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
}

// Closed reports whether Close has been called.
func (p *Planner) Closed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

func (p *Planner) plan(userOpts Options) (*ParsePlan, error) {
	ref, err := structValue(p.target)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrPlanClosed
	}
	p.nextGen++
	generation := p.nextGen
	// Clone under the lock so a concurrent commit cannot change the target
	// while the plan is being based on it.
	next, err := cloneValue(ref, map[cloneVisit]reflect.Value{})
	p.mu.Unlock()
	if err != nil {
		return nil, err
	}

	plan := &ParsePlan{
		generation: generation,
		target:     ref,
		next:       next,
	}
	plan.planner = p
	plan.err = fillPlan(plan, userOpts)
	return plan, plan.Err()
}

// parseAndApply is the two-phase implementation behind Parse and
// ParseWithOptions: plan into a private copy, and commit only if every field
// validated.
func parseAndApply(v interface{}, opts Options) error {
	plan, err := PlanWithOptions(v, opts)
	if err != nil {
		return err
	}
	return plan.Commit()
}

// Plan validates the environment into a private copy of v without modifying
// v. The plan is always returned so callers can inspect every field; when
// validation failed, the returned error is plan.Err().
func Plan(v interface{}) (*ParsePlan, error) {
	return PlanWithOptions(v, Options{})
}

// PlanWithOptions is like Plan but uses the given options.
func PlanWithOptions(v interface{}, opts Options) (*ParsePlan, error) {
	ref, err := structValue(v)
	if err != nil {
		return nil, err
	}

	next, err := cloneValue(ref, map[cloneVisit]reflect.Value{})
	if err != nil {
		return nil, err
	}

	plan := &ParsePlan{target: ref, next: next}
	plan.err = fillPlan(plan, opts)
	return plan, plan.Err()
}

func structValue(v interface{}) (reflect.Value, error) {
	ptrRef := reflect.ValueOf(v)
	if ptrRef.Kind() != reflect.Pointer {
		return reflect.Value{}, newAggregateError(NotStructPtrError{})
	}
	ref := ptrRef.Elem()
	if ref.Kind() != reflect.Struct {
		return reflect.Value{}, newAggregateError(NotStructPtrError{})
	}
	return ref, nil
}

// fillPlan runs the regular traversal against the private copy, recording
// what each field would become. It returns the underlying aggregate error,
// or nil when every field validated.
func fillPlan(plan *ParsePlan, userOpts Options) error {
	opts := customOptions(userOpts)
	plan.onSet = opts.OnSet
	opts.plan = &planRecorder{plan: plan}

	err := doParse(plan.next, planField, opts, "")
	if err == nil {
		return nil
	}
	if aggregate, ok := err.(AggregateError); ok {
		return aggregate
	}
	return newAggregateError(err)
}

type planRecorder struct {
	plan *ParsePlan
}

func (r *planRecorder) record(field FieldChange) {
	r.plan.fields = append(r.plan.fields, field)
}

func (r *planRecorder) addCall(key, value string, isDefault bool) {
	r.plan.calls = append(r.plan.calls, onSetCall{key: key, value: value, isDefault: isDefault})
}

func (r *planRecorder) addUnset(key string) {
	r.plan.unsets = append(r.plan.unsets, key)
}

// recordFieldError reports a field validation failure that happened before
// the value could be resolved, for instance an unsupported tag option.
func recordFieldError(opts Options, path string, err error) {
	if opts.plan == nil {
		return
	}
	opts.plan.record(FieldChange{
		Path: path,
		Err:  err,
	})
}

// planField resolves and validates the value, applies it to the private
// copy, and records a FieldChange without ever touching the destination
// struct.
func planField(
	refField reflect.Value,
	sf reflect.StructField,
	opts Options,
	fp FieldParams,
	path string,
) error {
	rec := opts.plan

	resolved, err := resolveValue(fp, opts)
	if fp.Unset && fp.OwnKey != "" {
		rec.addUnset(fp.Key)
	}
	if err != nil {
		if fp.OwnKey != "" {
			rec.record(FieldChange{
				Path:      path,
				Key:       fp.Key,
				IsDefault: resolved.isDefault,
				Err:       fieldError(fp.Key, resolved.value, opts, err),
			})
		}
		return err
	}

	if opts.OnSet != nil && fp.OwnKey != "" {
		rec.addCall(fp.Key, resolved.value, resolved.isDefault)
	}

	assign := resolved.value != "" &&
		(!resolved.isDefault || !opts.SetDefaultsForZeroValuesOnly || refField.IsZero())
	if !assign {
		if fp.OwnKey != "" {
			rec.record(FieldChange{
				Path:      path,
				Key:       fp.Key,
				IsDefault: resolved.isDefault,
				Old:       redactedFieldValue(refField, fp.Key, opts),
				New:       redactedFieldValue(refField, fp.Key, opts),
			})
		}
		return nil
	}

	old, oldErr := cloneValue(refField, map[cloneVisit]reflect.Value{})
	change := FieldChange{
		Path:        path,
		Key:         fp.Key,
		IsDefault:   resolved.isDefault,
		Old:         redactedFieldValue(refField, fp.Key, opts),
		Overwritten: !refField.IsZero(),
	}
	if err := set(refField, sf, resolved.value, opts.FuncMap); err != nil {
		if fp.OwnKey != "" {
			change.Err = fieldError(fp.Key, resolved.value, opts, err)
			change.New = change.Old
			rec.record(change)
		}
		return err
	}

	if fp.OwnKey != "" {
		change.Assigned = true
		change.New = redactedFieldValue(refField, fp.Key, opts)
		if oldErr == nil {
			change.Changed = !valuesEqual(old, refField)
		}
		rec.record(change)
	}
	return nil
}

func fieldError(key, raw string, opts Options, err error) error {
	return &FieldError{Key: key, Err: err, mask: isSensitiveKey(key, opts), raw: raw}
}

// cloneVisit identifies an already-cloned pointer, map or slice, so cyclic
// and shared values are copied without recursing forever.
type cloneVisit struct {
	typ    reflect.Type
	kind   reflect.Kind
	ptr    uintptr
	length int
}

// cloneValue deep-copies a value so planning cannot mutate anything reachable
// from the destination struct. Values parsing cannot write (funcs, channels,
// unexported fields) are shared by reference.
func cloneValue(v reflect.Value, seen map[cloneVisit]reflect.Value) (reflect.Value, error) {
	if !v.IsValid() {
		return v, nil
	}

	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return reflect.Zero(v.Type()), nil
		}
		key := cloneVisit{typ: v.Type(), kind: v.Kind(), ptr: v.Pointer()}
		if cloned, ok := seen[key]; ok {
			return cloned, nil
		}
		cloned := reflect.New(v.Type().Elem())
		seen[key] = cloned
		elem, err := cloneValue(v.Elem(), seen)
		if err != nil {
			return v, err
		}
		cloned.Elem().Set(elem)
		return cloned, nil

	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		out.Set(v)
		typ := v.Type()
		for i := 0; i < v.NumField(); i++ {
			if typ.Field(i).PkgPath != "" {
				continue
			}
			cloned, err := cloneValue(v.Field(i), seen)
			if err != nil {
				return v, err
			}
			out.Field(i).Set(cloned)
		}
		return out, nil

	case reflect.Slice:
		if v.IsNil() {
			return reflect.Zero(v.Type()), nil
		}
		// Slices sharing a backing array but with different lengths are
		// distinct values, so the length is part of the identity.
		key := cloneVisit{typ: v.Type(), kind: v.Kind(), ptr: v.Pointer(), length: v.Len()}
		if cloned, ok := seen[key]; ok {
			return cloned, nil
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		seen[key] = out
		for i := 0; i < v.Len(); i++ {
			cloned, err := cloneValue(v.Index(i), seen)
			if err != nil {
				return v, err
			}
			out.Index(i).Set(cloned)
		}
		return out, nil

	case reflect.Array:
		out := reflect.New(v.Type()).Elem()
		for i := 0; i < v.Len(); i++ {
			cloned, err := cloneValue(v.Index(i), seen)
			if err != nil {
				return v, err
			}
			out.Index(i).Set(cloned)
		}
		return out, nil

	case reflect.Map:
		if v.IsNil() {
			return reflect.Zero(v.Type()), nil
		}
		key := cloneVisit{typ: v.Type(), kind: v.Kind(), ptr: v.Pointer()}
		if cloned, ok := seen[key]; ok {
			return cloned, nil
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		seen[key] = out
		iter := v.MapRange()
		for iter.Next() {
			cloned, err := cloneValue(iter.Value(), seen)
			if err != nil {
				return v, err
			}
			out.SetMapIndex(iter.Key(), cloned)
		}
		return out, nil

	case reflect.Interface:
		if v.IsNil() {
			return reflect.Zero(v.Type()), nil
		}
		cloned, err := cloneValue(v.Elem(), seen)
		if err != nil {
			return v, err
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(cloned)
		return out, nil

	default:
		// Scalars are copied so diagnostics never alias the destination.
		out := reflect.New(v.Type()).Elem()
		out.Set(v)
		return out, nil
	}
}

// valuesEqual compares two values, treating funcs and channels by identity
// instead of reflect.DeepEqual's always-false rule.
func valuesEqual(a, b reflect.Value) bool {
	return valuesEqualSeen(a, b, map[valueVisit]bool{})
}

type valueVisit struct {
	a, b uintptr
	typ  reflect.Type
}

func valuesEqualSeen(a, b reflect.Value, seen map[valueVisit]bool) bool {
	if !a.IsValid() || !b.IsValid() {
		return a.IsValid() == b.IsValid()
	}
	if a.Type() != b.Type() {
		return false
	}

	switch a.Kind() {
	case reflect.Func, reflect.Chan, reflect.UnsafePointer:
		if a.IsNil() || b.IsNil() {
			return a.IsNil() == b.IsNil()
		}
		return a.Pointer() == b.Pointer()
	case reflect.Pointer:
		if a.IsNil() || b.IsNil() {
			return a.IsNil() == b.IsNil()
		}
		if a.Pointer() == b.Pointer() {
			return true
		}
		visit := valueVisit{a: a.Pointer(), b: b.Pointer(), typ: a.Type()}
		if seen[visit] {
			return true // already assumed equal while comparing this cycle
		}
		seen[visit] = true
		return valuesEqualSeen(a.Elem(), b.Elem(), seen)
	case reflect.Interface:
		if a.IsNil() || b.IsNil() {
			return a.IsNil() == b.IsNil()
		}
		return valuesEqualSeen(a.Elem(), b.Elem(), seen)
	case reflect.Struct:
		typ := a.Type()
		for i := 0; i < a.NumField(); i++ {
			// Unexported fields cannot be set by parsing, so they are never
			// part of a change and must not be read through Interface.
			if typ.Field(i).PkgPath != "" {
				continue
			}
			if !valuesEqualSeen(a.Field(i), b.Field(i), seen) {
				return false
			}
		}
		return true
	case reflect.Slice:
		if a.IsNil() != b.IsNil() || a.Len() != b.Len() {
			return false
		}
		if a.Len() > 0 && a.Pointer() == b.Pointer() {
			return true
		}
		for i := 0; i < a.Len(); i++ {
			if !valuesEqualSeen(a.Index(i), b.Index(i), seen) {
				return false
			}
		}
		return true
	case reflect.Array:
		for i := 0; i < a.Len(); i++ {
			if !valuesEqualSeen(a.Index(i), b.Index(i), seen) {
				return false
			}
		}
		return true
	case reflect.Map:
		if a.IsNil() != b.IsNil() || a.Len() != b.Len() {
			return false
		}
		if a.Len() > 0 && a.Pointer() == b.Pointer() {
			return true
		}
		iter := a.MapRange()
		for iter.Next() {
			bv := b.MapIndex(iter.Key())
			if !bv.IsValid() || !valuesEqualSeen(iter.Value(), bv, seen) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(a.Interface(), b.Interface())
	}
}

// redactedFieldValue renders a field value for diagnostics, masking it when
// the variable name looks sensitive.
func redactedFieldValue(v reflect.Value, key string, opts Options) string {
	return redactedValue(formatValue(v), key, opts)
}

func redactedValue(raw, key string, opts Options) string {
	if isSensitiveKey(key, opts) {
		return maskValue(raw)
	}
	return truncateValue(raw)
}

func truncateValue(raw string) string {
	const maxLen = 80
	if len(raw) <= maxLen {
		return raw
	}
	return raw[:maxLen] + "..."
}

func maskValue(raw string) string {
	if raw == "" {
		return `""`
	}
	return fmt.Sprintf("******(len=%d)", len(raw))
}

func isSensitiveKey(key string, opts Options) bool {
	if opts.RedactFunc != nil {
		return opts.RedactFunc(key)
	}
	upper := strings.ToUpper(key)
	for _, fragment := range sensitiveFragments {
		if strings.Contains(upper, fragment) {
			return true
		}
	}
	return false
}

var sensitiveFragments = []string{ //nolint:gochecknoglobals
	"SECRET",
	"PASSWORD",
	"PASSWD",
	"TOKEN",
	"API_KEY",
	"APIKEY",
	"PRIVATE_KEY",
	"PRIVATEKEY",
	"ACCESS_KEY",
	"ACCESSKEY",
	"CREDENTIAL",
	"AUTHORIZATION",
}

func formatValue(v reflect.Value) string {
	if !v.IsValid() {
		return "<unset>"
	}
	if !v.CanInterface() {
		return "<unexported>"
	}
	if v.Kind() == reflect.Pointer && v.IsNil() {
		return "<nil>"
	}
	if v.Kind() == reflect.String {
		return v.String()
	}
	return fmt.Sprintf("%v", v.Interface())
}

// PlanError reports a planning failure with the complete, redacted
// field-by-field diagnostics. It unwraps to the original AggregateError, so
// errors.Is and errors.As keep working.
type PlanError struct {
	Plan *ParsePlan
	Err  error
}

func (e *PlanError) Error() string {
	var sb strings.Builder
	sb.WriteString("env:")
	for _, f := range e.Plan.fields {
		if f.Err == nil {
			continue
		}
		fmt.Fprintf(&sb, " %v;", f.Err)
	}
	msg := strings.TrimRight(sb.String(), ";")
	if msg == "env:" && e.Err != nil {
		// Failures not attached to a field, e.g. an invalid tag option.
		return e.Err.Error()
	}
	return msg
}

// Unwrap exposes the underlying aggregate error.
func (e *PlanError) Unwrap() error {
	return e.Err
}

// FieldError is the redacted, per-field wrapper around a planning error. When
// the variable is sensitive, the raw value is replaced by a mask in the
// message; Unwrap still reaches the original error.
type FieldError struct {
	Key string
	Err error

	mask bool
	raw  string
}

func (e *FieldError) Error() string {
	msg := e.Err.Error()
	// Only conversion errors embed the offending value. Other errors (a
	// missing required variable, an unreadable file) carry names, not
	// secrets, so they are kept intact.
	if e.mask && e.raw != "" && embedsValue(e.Err) {
		for _, fragment := range redactionFragments(e.raw) {
			msg = strings.ReplaceAll(msg, fragment, maskValue(fragment))
		}
	}
	return msg
}

// redactionFragments lists the substrings a conversion error may echo: the
// whole raw value and each element of slices and maps. Empty and very short
// fragments are skipped, since replacing them would mangle the message.
func redactionFragments(raw string) []string {
	fragments := []string{raw}
	for _, sep := range []string{",", ";", "|"} {
		fragments = append(fragments, strings.Split(raw, sep)...)
	}
	for _, pair := range []string{":", "="} {
		parts := strings.Split(raw, pair)
		fragments = append(fragments, parts...)
		if len(parts) == 2 {
			fragments = append(fragments, parts[1])
		}
	}

	seen := make(map[string]bool, len(fragments))
	out := make([]string, 0, len(fragments))
	for _, f := range fragments {
		if len(f) < 3 || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

func embedsValue(err error) bool {
	var parseErr ParseError
	if errors.As(err, &parseErr) {
		return true
	}
	var valueErr ParseValueError
	return errors.As(err, &valueErr)
}

// Unwrap exposes the original error.
func (e *FieldError) Unwrap() error {
	return e.Err
}

// ApplyError reports a failure while copying a plan into the target. The
// target keeps its previous values.
type ApplyError struct {
	Err error
}

func (e *ApplyError) Error() string {
	return fmt.Sprintf("env: could not apply plan: %v", e.Err)
}

// Unwrap exposes the underlying cause.
func (e *ApplyError) Unwrap() error {
	return e.Err
}

// StalePlanError reports a generation that lost against a newer commit.
type StalePlanError struct {
	Generation uint64
	Committed  uint64
}

func (e *StalePlanError) Error() string {
	return fmt.Sprintf(
		"env: plan generation %d is older than committed generation %d",
		e.Generation, e.Committed,
	)
}

// Unwrap implements errors.Is against ErrStalePlan.
func (e *StalePlanError) Unwrap() error {
	return ErrStalePlan
}

func joinFieldPath(parent, field string) string {
	if parent == "" {
		return field
	}
	return parent + "." + field
}

func indexFieldPath(parent string, index int) string {
	return fmt.Sprintf("%s[%d]", parent, index)
}
