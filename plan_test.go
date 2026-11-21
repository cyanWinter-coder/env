package env

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
)

type planInner struct {
	Host string `env:"HOST" envDefault:"localhost"`
	Port int    `env:"PORT" envDefault:"8080"`
}

type planCustom struct {
	value string
}

func (c *planCustom) UnmarshalText(b []byte) error {
	c.value = "custom:" + string(b)
	return nil
}

type planConfig struct {
	Name     string         `env:"NAME" envDefault:"default-name"`
	Password string         `env:"PASSWORD"`
	Token    string         `env:"TOKEN"`
	Inner    planInner      `envPrefix:"INNER_"`
	Custom   planCustom     `env:"CUSTOM"`
	Tags     []string       `env:"TAGS" envSeparator:";"`
	Counts   map[string]int `env:"COUNTS"`
	Number   int            `env:"NUMBER"`
}

func TestPlanDoesNotModifyTarget(t *testing.T) {
	cfg := planConfig{Name: "keep", Inner: planInner{Host: "old", Port: 1}}
	before := cfg

	t.Setenv("INNER_PORT", "9090")
	t.Setenv("PASSWORD", "s3cr3t-value")
	t.Setenv("TAGS", "a;b")
	t.Setenv("COUNTS", "x:1,y:2")
	t.Setenv("CUSTOM", "raw")
	t.Setenv("NUMBER", "7")

	plan, err := PlanWithOptions(&cfg, Options{Environment: map[string]string{
		"INNER_PORT": "9090",
		"PASSWORD":   "s3cr3t-value",
		"TAGS":       "a;b",
		"COUNTS":     "x:1,y:2",
		"CUSTOM":     "raw",
		"NUMBER":     "7",
	}})
	isNoErr(t, err)
	isTrue(t, plan.HasErrors() == false)
	isEqual(t, before, cfg)

	// All the resolved values are in the plan, already validated.
	isEqual(t, "default-name", plan.ChangesMap()["Name"].New)
	isEqual(t, "9090", plan.ChangesMap()["Inner.Port"].New)
	isEqual(t, "localhost", plan.ChangesMap()["Inner.Host"].New)

	isNoErr(t, plan.Apply())
	isEqual(t, "default-name", cfg.Name)
	isEqual(t, "s3cr3t-value", cfg.Password)
	isEqual(t, "localhost", cfg.Inner.Host)
	isEqual(t, 9090, cfg.Inner.Port)
	isEqual(t, "custom:raw", cfg.Custom.value)
	isEqual(t, []string{"a", "b"}, cfg.Tags)
	isEqual(t, map[string]int{"x": 1, "y": 2}, cfg.Counts)
	isEqual(t, 7, cfg.Number)
	isTrue(t, plan.Applied())
}

func TestPlanFailureLeavesTargetUntouched(t *testing.T) {
	cfg := planConfig{Name: "keep-me", Number: 42}
	before := cfg

	t.Setenv("NAME", "new-name")
	t.Setenv("NUMBER", "not-a-number")

	plan, err := Plan(&cfg)
	isTrue(t, err != nil)
	isTrue(t, plan.HasErrors())
	isEqual(t, before, cfg)

	// Applying a failed plan must not change anything either.
	err = plan.Apply()
	isTrue(t, err != nil)
	isEqual(t, before, cfg)
}

func TestPlanReportsEveryFieldAndSource(t *testing.T) {
	cfg := planConfig{}
	t.Setenv("INNER_PORT", "1234")
	t.Setenv("NAME", "from-env")

	plan, err := PlanWithOptions(&cfg, Options{Environment: map[string]string{
		"INNER_PORT": "1234",
		"NAME":       "from-env",
	}})
	isNoErr(t, err)

	fields := plan.Fields()
	byPath := make(map[string]FieldChange, len(fields))
	for _, f := range fields {
		byPath[f.Path] = f
	}

	name, ok := byPath["Name"]
	isTrue(t, ok)
	isEqual(t, "NAME", name.Key)
	isEqual(t, "from-env", name.New)
	isFalse(t, name.IsDefault)
	isTrue(t, name.Assigned)

	port, ok := byPath["Inner.Port"]
	isTrue(t, ok)
	isEqual(t, "INNER_PORT", port.Key)
	isEqual(t, "1234", port.New)
	isFalse(t, port.IsDefault)

	host, ok := byPath["Inner.Host"]
	isTrue(t, ok)
	isEqual(t, "INNER_HOST", host.Key)
	isTrue(t, host.IsDefault)

	password, ok := byPath["Password"]
	isTrue(t, ok)
	isEqual(t, "PASSWORD", password.Key)
	isFalse(t, password.Changed)
}

func TestPlanRedactsSensitiveValues(t *testing.T) {
	cfg := planConfig{}
	t.Setenv("PASSWORD", "super-secret-password")
	t.Setenv("TOKEN", "tok_live_123456")

	plan, err := Plan(&cfg)
	isNoErr(t, err)

	for _, f := range plan.Fields() {
		switch f.Path {
		case "Password", "Token":
			isFalse(t, strings.Contains(f.New, "super-secret"))
			isFalse(t, strings.Contains(f.New, "tok_live"))
			isTrue(t, strings.Contains(f.New, "******"))
		}
	}

	rendered := plan.String()
	isFalse(t, strings.Contains(rendered, "super-secret-password"))
	isFalse(t, strings.Contains(rendered, "tok_live_123456"))
	isTrue(t, strings.Contains(rendered, "PASSWORD"))
}

func TestPlanRedactsValuesInErrors(t *testing.T) {
	type cfg struct {
		Password []int `env:"PASSWORD_INT" envDefault:"secret,values"`
	}
	t.Setenv("PASSWORD_INT", "secret,values")

	plan, err := Plan(&cfg{})
	isTrue(t, err != nil)
	isTrue(t, plan.HasErrors())

	msg := err.Error()
	isFalse(t, strings.Contains(msg, "secret"))
	isFalse(t, strings.Contains(msg, "values"))
	isTrue(t, strings.Contains(msg, "******"))
	isTrue(t, errors.Is(err, ParseError{}))

	var parseErr ParseError
	isTrue(t, errors.As(err, &parseErr))
}

func TestPlanRedactFunc(t *testing.T) {
	type cfg struct {
		Public string `env:"PUBLIC"`
	}
	t.Setenv("PUBLIC", "sensitive")

	plan, err := PlanWithOptions(&cfg{}, Options{
		RedactFunc: func(key string) bool { return key == "PUBLIC" },
	})
	isNoErr(t, err)
	field := plan.ChangesMap()["Public"]
	isEqual(t, "******(len=9)", field.New)
}

func TestPlanRequiredAndNotEmpty(t *testing.T) {
	type cfg struct {
		Required string `env:"PLAN_REQUIRED_REQUIRED,required"`
		NotEmpty string `env:"PLAN_REQUIRED_EMPTY,notEmpty"`
	}
	t.Setenv("PLAN_REQUIRED_EMPTY", "")

	plan, err := Plan(&cfg{})
	isTrue(t, err != nil)
	isTrue(t, errors.Is(err, VarIsNotSetError{}))
	isTrue(t, errors.Is(err, EmptyVarError{}))
	isTrue(t, plan.HasErrors())

	// The complete diagnostics list every failing field.
	var planErr *PlanError
	isTrue(t, errors.As(err, &planErr))
	isTrue(t, len(plan.Fields()) >= 2)

	msg := err.Error()
	isTrue(t, strings.Contains(msg, "PLAN_REQUIRED_REQUIRED"))
	isTrue(t, strings.Contains(msg, "PLAN_REQUIRED_EMPTY"))
}

func TestPlanNestedRequiredStillDiagnosesAllFields(t *testing.T) {
	type inner struct {
		Host string `env:"HOST,required"`
		Port int    `env:"PORT"`
	}
	type cfg struct {
		Server inner  `envPrefix:"SERVER_"`
		Name   string `env:"NAME,required"`
	}
	t.Setenv("SERVER_PORT", "12")

	plan, err := Plan(&cfg{})
	isTrue(t, err != nil)
	paths := make([]string, 0, len(plan.Fields()))
	for _, f := range plan.Fields() {
		paths = append(paths, f.Path)
	}
	isEqual(t, []string{"Server.Host", "Server.Port", "Name"}, paths)

	// The valid field is still fully resolved even though siblings failed.
	var port FieldChange
	for _, f := range plan.Fields() {
		if f.Path == "Server.Port" {
			port = f
		}
	}
	isEqual(t, "12", port.New)
	isNoErr(t, port.Err)
}

func TestParseIsAllOrNothing(t *testing.T) {
	type cfg struct {
		A string `env:"A"`
		B int    `env:"B"`
	}
	c := cfg{A: "old", B: 1}
	t.Setenv("A", "new")
	t.Setenv("B", "broken")

	err := Parse(&c)
	isTrue(t, err != nil)
	isEqual(t, cfg{A: "old", B: 1}, c)

	t.Setenv("B", "2")
	isNoErr(t, Parse(&c))
	isEqual(t, cfg{A: "new", B: 2}, c)
}

func TestPlanSeparatorAndPointerDefaults(t *testing.T) {
	type cfg struct {
		Flag  *bool    `env:"FLAG,init"`
		Ports *[]int   `env:"PORTS"`
		List  []string `env:"LIST" envSeparator:"|"`
	}
	c := cfg{Ports: &[]int{9}}
	plan, err := PlanWithOptions(&c, Options{Environment: map[string]string{
		"LIST": "a|b|c",
	}})
	isNoErr(t, err)
	isNoErr(t, plan.Apply())

	isEqual(t, false, *c.Flag)
	isEqual(t, []int{9}, *c.Ports)
	isEqual(t, []string{"a", "b", "c"}, c.List)
}

func TestPlanCustomParserAndEnum(t *testing.T) {
	type config struct {
		Value planCustom `env:"CUSTOM"`
	}
	t.Setenv("CUSTOM", "hello")
	c := config{}
	plan, err := Plan(&c)
	isNoErr(t, err)
	isNoErr(t, plan.Apply())
	isEqual(t, "custom:hello", c.Value.value)
}

func TestPlanOnSetAfterCommitOnly(t *testing.T) {
	type cfg struct {
		A string `env:"A"`
		B int    `env:"B"`
	}
	t.Setenv("A", "a")
	t.Setenv("B", "11")
	c := cfg{}

	var calls []string
	opts := Options{OnSet: func(key string, value interface{}, isDefault bool) {
		// The destination must already hold the value when OnSet runs.
		calls = append(calls, key)
	}}

	plan, err := PlanWithOptions(&c, opts)
	isNoErr(t, err)
	isEqual(t, 0, len(calls))
	isNoErr(t, plan.Commit())
	isEqual(t, []string{"A", "B"}, calls)
	isEqual(t, "a", c.A)
}

func TestPlanOnSetSkippedWhenCommitFails(t *testing.T) {
	type cfg struct {
		A string `env:"A"`
		B int    `env:"B"`
	}
	t.Setenv("A", "a")
	t.Setenv("B", "nope")
	c := cfg{}

	called := false
	plan, err := PlanWithOptions(&c, Options{OnSet: func(string, interface{}, bool) { called = true }})
	isTrue(t, err != nil)
	isTrue(t, plan.HasErrors())
	isTrue(t, plan.Apply() != nil)
	isFalse(t, called)
	isEqual(t, cfg{}, c)
}

func TestPlanUnsetOnlyAfterCommit(t *testing.T) {
	type cfg struct {
		Secret string `env:"PLAN_UNSET,unset"`
	}
	t.Setenv("PLAN_UNSET", "value")
	c := cfg{}

	plan, err := Plan(&c)
	isNoErr(t, err)
	_, stillSet := os.LookupEnv("PLAN_UNSET")
	isTrue(t, stillSet)

	isNoErr(t, plan.Apply())
	isEqual(t, "value", c.Secret)
	_, stillSet = os.LookupEnv("PLAN_UNSET")
	isFalse(t, stillSet)
}

func TestPlannerGenerationOrdering(t *testing.T) {
	type cfg struct {
		Value string `env:"VALUE"`
	}
	t.Setenv("VALUE", "one")
	target := cfg{}
	planner := NewPlanner(&target)

	first, err := planner.Plan()
	isNoErr(t, err)
	isEqual(t, uint64(1), first.Generation())

	second, err := planner.Plan()
	isNoErr(t, err)
	isEqual(t, uint64(2), second.Generation())

	// The newer plan wins.
	isNoErr(t, planner.Commit(second))
	isEqual(t, "one", target.Value)
	isEqual(t, uint64(2), planner.Generation())

	// The older plan arriving late must not overwrite it.
	err = planner.Commit(first)
	isTrue(t, err != nil)
	isTrue(t, errors.Is(err, ErrStalePlan))
	isEqual(t, uint64(2), planner.Generation())
	isEqual(t, "one", target.Value)
}

func TestPlannerStalePlanKeepsNewerValue(t *testing.T) {
	type cfg struct {
		Value int `env:"VALUE"`
	}
	target := cfg{Value: 1}
	planner := NewPlanner(&target)

	oldPlan, err := planner.PlanWithOptions(Options{Environment: map[string]string{"VALUE": "2"}})
	isNoErr(t, err)

	newPlan, err := planner.PlanWithOptions(Options{Environment: map[string]string{"VALUE": "3"}})
	isNoErr(t, err)

	isNoErr(t, planner.Commit(newPlan))
	isEqual(t, 3, target.Value)

	err = planner.Commit(oldPlan)
	isTrue(t, errors.Is(err, ErrStalePlan))
	isEqual(t, 3, target.Value)
}

func TestPlannerConcurrentPlansAreReadOnly(t *testing.T) {
	type cfg struct {
		A string `env:"A"`
		B string `env:"B"`
	}
	target := cfg{A: "stable", B: "stable"}
	before := target
	planner := NewPlanner(&target)

	var wg sync.WaitGroup
	plans := make([]*ParsePlan, 16)
	for i := range plans {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			plan, err := planner.Plan()
			if err != nil {
				t.Errorf("plan %d: %v", i, err)
				return
			}
			plans[i] = plan
		}(i)
	}
	wg.Wait()

	isEqual(t, before, target)
	for _, plan := range plans {
		isTrue(t, plan != nil)
	}
}

func TestPlannerConcurrentCommitsKeepLastGeneration(t *testing.T) {
	type cfg struct {
		Value int `env:"VALUE"`
	}
	target := cfg{}
	planner := NewPlannerWithOptions(&target, Options{Environment: map[string]string{"VALUE": "5"}})

	const n = 20
	plans := make([]*ParsePlan, n)
	for i := range plans {
		plan, err := planner.Plan()
		isNoErr(t, err)
		plans[i] = plan
	}

	var wg sync.WaitGroup
	for _, plan := range plans {
		wg.Add(1)
		go func(plan *ParsePlan) {
			defer wg.Done()
			_ = planner.Commit(plan)
		}(plan)
	}
	wg.Wait()

	isEqual(t, 5, target.Value)
	isTrue(t, planner.Generation() > 0)
}

func TestPlannerCloseStopsWrites(t *testing.T) {
	type cfg struct {
		Value string `env:"VALUE"`
	}
	t.Setenv("VALUE", "new")
	target := cfg{Value: "old"}
	planner := NewPlanner(&target)

	plan, err := planner.Plan()
	isNoErr(t, err)

	planner.Close()
	isTrue(t, planner.Closed())

	err = planner.Commit(plan)
	isTrue(t, errors.Is(err, ErrPlanClosed))
	isEqual(t, "old", target.Value)

	_, err = planner.Plan()
	isTrue(t, errors.Is(err, ErrPlanClosed))
}

func TestPlanCloseDiscardsPlan(t *testing.T) {
	type cfg struct {
		Value string `env:"VALUE"`
	}
	t.Setenv("VALUE", "new")
	target := cfg{Value: "old"}

	plan, err := Plan(&target)
	isNoErr(t, err)
	plan.Close()

	err = plan.Apply()
	isTrue(t, errors.Is(err, ErrPlanClosed))
	isEqual(t, "old", target.Value)
}

func TestPlanRejectsForeignPlanner(t *testing.T) {
	type cfg struct {
		Value string `env:"VALUE"`
	}
	target := cfg{}
	owner := NewPlanner(&target)
	other := NewPlanner(&target)

	plan, err := owner.Plan()
	isNoErr(t, err)

	err = other.Commit(plan)
	isTrue(t, errors.Is(err, ErrInvalidPlan))
	isNoErr(t, owner.Commit(plan))
}

func TestPlanIdempotentCommit(t *testing.T) {
	type cfg struct {
		Value string `env:"VALUE"`
	}
	t.Setenv("VALUE", "v")
	target := cfg{}
	planner := NewPlanner(&target)

	plan, err := planner.Plan()
	isNoErr(t, err)
	isNoErr(t, planner.Commit(plan))
	isNoErr(t, planner.Commit(plan)) // double publish is a no-op
	isEqual(t, "v", target.Value)
	isEqual(t, uint64(1), planner.Generation())

	isNoErr(t, plan.Apply()) // Apply after a planner commit is also a no-op
	isEqual(t, "v", target.Value)
}

func TestPlannerReloadIsAllOrNothing(t *testing.T) {
	type cfg struct {
		Name string `env:"NAME"`
		Port int    `env:"PORT"`
	}
	target := cfg{Name: "keep", Port: 1}
	planner := NewPlannerWithOptions(&target, Options{Environment: map[string]string{
		"NAME": "new",
		"PORT": "broken",
	}})

	err := planner.Reload()
	isTrue(t, err != nil)
	isEqual(t, cfg{Name: "keep", Port: 1}, target)

	// Fix the environment and reload again.
	planner.opts.Environment["PORT"] = "2"
	isNoErr(t, planner.Reload())
	isEqual(t, cfg{Name: "new", Port: 2}, target)
}

func TestPlannerRejectsNonStructPointer(t *testing.T) {
	planner := NewPlanner(42)
	_, err := planner.Plan()
	isTrue(t, errors.Is(err, NotStructPtrError{}))

	planner = NewPlanner(&planConfig{})
	_, err = planner.Plan()
	isNoErr(t, err)
}

func TestPlanDoesNotAliasPointerValues(t *testing.T) {
	type inner struct {
		Host string `env:"HOST"`
	}
	type cfg struct {
		Inner *inner `env:",init"`
	}
	original := &inner{Host: "keep"}
	target := cfg{Inner: original}
	t.Setenv("HOST", "changed")

	plan, err := Plan(&target)
	isNoErr(t, err)
	isEqual(t, "keep", original.Host)
	isNoErr(t, plan.Apply())
	isEqual(t, "changed", target.Inner.Host)
	isEqual(t, "keep", original.Host)
}

func TestPlanNestedSliceDiagnostics(t *testing.T) {
	type item struct {
		Name string `env:"NAME"`
		Port int    `env:"PORT"`
	}
	type cfg struct {
		Items []item `envPrefix:"ITEMS_"`
	}
	t.Setenv("ITEMS_0_NAME", "first")
	t.Setenv("ITEMS_1_PORT", "84")

	c := cfg{}
	plan, err := Plan(&c)
	isNoErr(t, err)
	paths := make([]string, 0, len(plan.Fields()))
	for _, f := range plan.Fields() {
		paths = append(paths, f.Path)
	}
	isEqual(t, []string{"Items[0].Name", "Items[0].Port", "Items[1].Name", "Items[1].Port"}, paths)
	isNoErr(t, plan.Apply())
	isEqual(t, "first", c.Items[0].Name)
	isEqual(t, 84, c.Items[1].Port)
}

func TestPlanOverwrittenFlag(t *testing.T) {
	type cfg struct {
		Keep string `env:"KEEP"`
	}
	t.Setenv("KEEP", "new")
	target := cfg{Keep: "old"}
	plan, err := Plan(&target)
	isNoErr(t, err)
	field := plan.ChangesMap()["Keep"]
	isTrue(t, field.Overwritten)
	isTrue(t, field.Changed)
	isEqual(t, "old", field.Old)
	isEqual(t, "new", field.New)
}

func TestPlanChangesAndApplyAlias(t *testing.T) {
	type cfg struct {
		Keep  string `env:"KEEP"`
		Fresh string `env:"FRESH"`
	}
	target := cfg{Keep: "same"}
	plan, err := PlanWithOptions(&target, Options{Environment: map[string]string{
		"KEEP":  "same",
		"FRESH": "new",
	}})
	isNoErr(t, err)

	changes := plan.Changes()
	isEqual(t, 1, len(changes))
	isEqual(t, "Fresh", changes[0].Path)
	isEqual(t, "FRESH", changes[0].Key)
	isFalse(t, changes[0].IsDefault)

	isNoErr(t, plan.Apply())
	isEqual(t, "same", target.Keep)
	isEqual(t, "new", target.Fresh)
}

func TestPlanErrorFailureOutsideField(t *testing.T) {
	type cfg struct {
		A string `env:"A,bogus"`
	}
	plan, err := Plan(&cfg{})
	isTrue(t, err != nil)
	isTrue(t, errors.Is(err, NoSupportedTagOptionError{}))
	isTrue(t, plan.HasErrors())
	isTrue(t, strings.Contains(err.Error(), "bogus"))
}

func TestPlanSliceAndMapParsing(t *testing.T) {
	type cfg struct {
		Numbers []int           `env:"NUMBERS"`
		Lookup  map[string]bool `env:"LOOKUP" envSeparator:";" envKeyValSeparator:"="`
	}
	c := cfg{}
	plan, err := PlanWithOptions(&c, Options{Environment: map[string]string{
		"NUMBERS": "1,2,3",
		"LOOKUP":  "a=true;b=false",
	}})
	isNoErr(t, err)
	isNoErr(t, plan.Apply())
	isEqual(t, []int{1, 2, 3}, c.Numbers)
	isEqual(t, map[string]bool{"a": true, "b": false}, c.Lookup)
}

func TestPlanTextUnmarshalerAndDefaults(t *testing.T) {
	type cfg struct {
		Level LogLevel `env:"LEVEL" envDefault:"debug"`
	}
	c := cfg{}
	plan, err := PlanWithOptions(&c, Options{Environment: map[string]string{}})
	isNoErr(t, err)
	field := plan.ChangesMap()["Level"]
	isTrue(t, field.IsDefault)
	isNoErr(t, plan.Apply())
	isEqual(t, DebugLevel, c.Level)
}

func TestPlanStaleApplyKeepsNewerTarget(t *testing.T) {
	type cfg struct {
		Value string `env:"VALUE"`
	}
	target := cfg{}
	planner := NewPlanner(&target)

	older, err := planner.PlanWithOptions(Options{Environment: map[string]string{"VALUE": "old"}})
	isNoErr(t, err)
	newer, err := planner.PlanWithOptions(Options{Environment: map[string]string{"VALUE": "new"}})
	isNoErr(t, err)

	isNoErr(t, planner.Commit(newer))
	err = older.Commit()
	isTrue(t, errors.Is(err, ErrStalePlan))
	isEqual(t, "new", target.Value)
}

func TestStalePlanErrorDetails(t *testing.T) {
	type cfg struct {
		Value string `env:"VALUE"`
	}
	target := cfg{}
	planner := NewPlanner(&target)
	first, err := planner.Plan()
	isNoErr(t, err)
	second, err := planner.Plan()
	isNoErr(t, err)
	isNoErr(t, planner.Commit(second))

	err = planner.Commit(first)
	var stale *StalePlanError
	isTrue(t, errors.As(err, &stale))
	isEqual(t, uint64(1), stale.Generation)
	isEqual(t, uint64(2), stale.Committed)
	isTrue(t, strings.Contains(err.Error(), "generation 1"))
}

func TestPlanErrorTypes(t *testing.T) {
	inner := errors.New("boom")

	fieldErr := &FieldError{Key: "PASSWORD", Err: inner, mask: true, raw: "super-secret"}
	isEqual(t, "boom", fieldErr.Error())
	isTrue(t, errors.Is(fieldErr, inner))

	applyErr := &ApplyError{Err: inner}
	isTrue(t, errors.Is(applyErr, inner))
	isTrue(t, strings.Contains(applyErr.Error(), "boom"))

	stale := &StalePlanError{Generation: 1, Committed: 2}
	isTrue(t, errors.Is(stale, ErrStalePlan))
}

func TestPlanRedactsSliceElementValues(t *testing.T) {
	type cfg struct {
		Passwords []int `env:"PASSWORDS" envDefault:"aaa-secret,bbb-secret,not-an-int"`
	}
	plan, err := PlanWithOptions(&cfg{}, Options{Environment: map[string]string{
		"PASSWORDS": "aaa-secret,bbb-secret,not-an-int",
	}})
	isTrue(t, err != nil)
	isTrue(t, plan.HasErrors())
	isFalse(t, strings.Contains(err.Error(), "aaa-secret"))
	isFalse(t, strings.Contains(err.Error(), "bbb-secret"))
	isFalse(t, strings.Contains(err.Error(), "not-an-int"))
}

func TestPlanPreservesDefaultsOnKeylessFields(t *testing.T) {
	type cfg struct {
		WithDefault string `envDefault:"fallback"`
	}
	c := cfg{}
	plan, err := PlanWithOptions(&c, Options{Environment: map[string]string{}})
	isNoErr(t, err)
	isNoErr(t, plan.Apply())
	isEqual(t, "fallback", c.WithDefault)
}

func TestPlannerApplyAlias(t *testing.T) {
	type cfg struct {
		Value string `env:"VALUE"`
	}
	target := cfg{}
	planner := NewPlannerWithOptions(&target, Options{Environment: map[string]string{"VALUE": "v"}})
	plan, err := planner.Plan()
	isNoErr(t, err)
	isNoErr(t, planner.Apply(plan))
	isEqual(t, "v", target.Value)

	isTrue(t, planner.Apply(nil) != nil)
}

func TestPlanHandlesArraysAndInterfaces(t *testing.T) {
	type cfg struct {
		Arr [3]int `env:"ARR"`
		Any any
	}
	target := cfg{Arr: [3]int{1, 2, 3}, Any: "before"}
	plan, err := PlanWithOptions(&target, Options{Environment: map[string]string{"ARR": "x"}})
	isTrue(t, err != nil) // arrays have no parser
	isEqual(t, [3]int{1, 2, 3}, target.Arr)
	isEqual(t, "before", target.Any)
	isTrue(t, plan.Fields()[0].Err != nil)
	isTrue(t, plan.Apply() != nil)
	isEqual(t, [3]int{1, 2, 3}, target.Arr)
}
