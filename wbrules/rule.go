package wbrules

import (
	"fmt"
	"log"
	"time"
	"errors"
	"strconv"
	"strings"
	"github.com/stretchr/objx"
	"github.com/GeertJohan/go.rice"
	duktape "github.com/ivan4th/go-duktape"
	wbgo "github.com/contactless/wbgo"
)

type esCallback uint64

const (
	LIB_FILE = "lib.js"
	DEFAULT_CELL_MAX = 255.0
	TIMERS_CAPACITY = 128
	RULES_CAPACITY = 256
	CELL_RULES_CAPACITY = 8
	NO_CALLBACK = esCallback(0)
	RULE_TYPE_NONE = iota
	RULE_TYPE_LEVEL_TRIGGERED
	RULE_TYPE_EDGE_TRIGGERED
	RULE_TYPE_ON_CELL_CHANGE
)

type RuleType int

type Timer interface {
	GetChannel() <-chan time.Time
	Stop()
}
type TimerFunc func (id int, d time.Duration, periodic bool) Timer

type RealTicker struct {
	innerTicker *time.Ticker
}

func (ticker *RealTicker) GetChannel() <-chan time.Time {
	if ticker.innerTicker == nil {
		panic("trying to get channel from a stopped ticker")
	}
	return ticker.innerTicker.C
}

func (ticker *RealTicker) Stop() {
	if ticker.innerTicker != nil {
		ticker.innerTicker.Stop()
		ticker.innerTicker = nil
	}
}

type RealTimer struct {
	innerTimer *time.Timer
}

func (timer *RealTimer) GetChannel() <-chan time.Time {
	if timer.innerTimer == nil {
		panic("trying to get channel from a stopped timer")
	}
	return timer.innerTimer.C
}

func (timer *RealTimer) Stop() {
	if timer.innerTimer != nil {
		timer.innerTimer.Stop()
		timer.innerTimer = nil
	}
}

func newTimer(id int, d time.Duration, periodic bool) Timer {
	if periodic {
		return &RealTicker{time.NewTicker(d)}
	} else {
		return &RealTimer{time.NewTimer(d)}
	}
}

type TimerEntry struct {
	timer Timer
	periodic bool
	quit chan struct{}
}

type Rule struct {
	engine *RuleEngine
	name string
	cond esCallback
	then esCallback
	onCellChange []*Cell
	ruleType RuleType
	firstRun bool
	prevCondValue bool
	oldCellValue interface{}
	shouldCheck bool
}

// TBD: reduce the spaghetti, use distinct Rule subtypes
func newRule(engine *RuleEngine, name string, defIndex int) (*Rule, error) {
	rule := &Rule{
		engine: engine,
		name: name,
		cond: 0,
		then: 0,
		firstRun: true,
		prevCondValue: false,
		oldCellValue: nil,
		shouldCheck: false,
	}
	ctx := engine.ctx

	if !ctx.HasPropString(defIndex, "then") {
		// this should be handled by lib.js
		return nil, errors.New("invalid rule -- no then")
	}
	rule.then = rule.storeCallback(defIndex, "then")
	hasWhen := ctx.HasPropString(defIndex, "when")
	hasAsSoonAs := ctx.HasPropString(defIndex, "asSoonAs")
	hasOnCellChange := ctx.HasPropString(defIndex, "onCellChange")

	if hasWhen {
		if hasAsSoonAs || hasOnCellChange {
			return nil, errors.New(
				"invalid rule -- cannot combine 'when' with 'asSoonAs' or 'onCellChange'")
		}
		rule.cond = rule.storeCallback(defIndex, "when")
		rule.ruleType = RULE_TYPE_LEVEL_TRIGGERED
	} else if hasAsSoonAs {
		if hasOnCellChange {
			return nil, errors.New(
				"invalid rule -- cannot combine 'asSoonAs' with 'onCellChange'")
		}
		rule.cond = rule.storeCallback(defIndex, "asSoonAs")
		rule.ruleType = RULE_TYPE_EDGE_TRIGGERED
	} else if hasOnCellChange {
		ctx.GetPropString(defIndex, "onCellChange")
		var cellNames []string
		if ctx.IsString(-1) {
			cellNames = []string{ ctx.ToString(-1) }
		} else {
			cellNames = StringArrayToGo(ctx, -1)
			if len(cellNames) == 0 {
				return nil, errors.New("empty onCellChange")
			}
		}
		ctx.Pop()
		rule.ruleType = RULE_TYPE_ON_CELL_CHANGE
		rule.onCellChange = make([]*Cell, len(cellNames))
		for i, fullName := range cellNames {
			parts := strings.SplitN(fullName, "/", 2)
			if len(parts) != 2 {
				return nil, fmt.Errorf("invalid onCellChange spec: '%s'", fullName)
			}
			cell := engine.getCell(parts[0], parts[1])
			rule.onCellChange[i] = cell
			engine.storeRuleCell(rule, cell)
		}
	} else {
		return nil, errors.New(
			"invalid rule -- must provide one of 'when', 'asSoonAs' or 'onCellChange'")
	}

	return rule, nil
}

func (rule *Rule) invokeCond() bool {
	rule.engine.startTrackingCells()
	defer rule.engine.storeRuleTrackedCells(rule)
	return rule.engine.invokeCallback("ruleFuncs", rule.cond, nil)
}

func (rule *Rule) ShouldCheck() {
	rule.shouldCheck = true
}

func (rule *Rule) Check(cell *Cell) {
	if cell != nil && !rule.shouldCheck {
		// Don't invoke js if no cells mentioned in the
		// condition callback changed. If rules are run
		// not due to a cell being changed, still need
		// to call JS though.
		return
	}
	if rule.ruleType == RULE_TYPE_NONE {
		panic("invoking a destroyed rule")
	}
	shouldFire := false
	args := objx.Map(nil)
	switch (rule.ruleType) {
	case RULE_TYPE_LEVEL_TRIGGERED:
		shouldFire = rule.invokeCond()
	case RULE_TYPE_EDGE_TRIGGERED:
		current := rule.invokeCond()
		shouldFire = current && (rule.firstRun || current != rule.prevCondValue)
		rule.prevCondValue = current
	case RULE_TYPE_ON_CELL_CHANGE:
		if cell != nil && cell.IsComplete() {
			for _, checkCell := range rule.onCellChange {
				if cell == checkCell {
					shouldFire = true
					args = objx.New(map[string]interface{} {
						"device": cell.DevName(),
						"cell": cell.Name(),
						"newValue": cell.Value(),
						"oldValue": rule.oldCellValue,
					})
					rule.oldCellValue = cell.Value()
					break
				}
			}
		}
	}

	rule.firstRun = false
	rule.shouldCheck = false
	if shouldFire {
		rule.engine.invokeCallback("ruleFuncs", rule.then, args)
	}
}

func (rule *Rule) storeCallback(defIndex int, propName string) esCallback {
	rule.engine.ctx.GetPropString(defIndex, propName)
	defer rule.engine.ctx.Pop()
	return rule.engine.storeCallback("ruleFuncs", -1, nil)
}

func (rule *Rule) Destroy() {
	if rule.cond != 0 {
		rule.engine.removeCallback("ruleFuncs", rule.cond)
	}
	if rule.then != 0{
		rule.engine.removeCallback("ruleFuncs", rule.then)
	}
	rule.ruleType = RULE_TYPE_NONE
}

type LogFunc func (string)

type RuleEngine struct {
	model *CellModel
	mqttClient wbgo.MQTTClient
	ctx *duktape.Context
	logFunc LogFunc
	cellChange chan *CellSpec
	scriptBox *rice.Box
	timerFunc TimerFunc
	timers []*TimerEntry
	callbackIndex esCallback
	ruleMap map[string]*Rule
	ruleList []string
	notedCells map[*Cell]bool
	cellToRuleMap map[*Cell][]*Rule
	rulesWithoutCells map[*Rule]bool
}

func NewRuleEngine(model *CellModel, mqttClient wbgo.MQTTClient) (engine *RuleEngine) {
	engine = &RuleEngine{
		model: model,
		mqttClient: mqttClient,
		ctx: duktape.NewContext(),
		logFunc: func (message string) {
			wbgo.Info.Printf("RULE: %s\n", message)
		},
		scriptBox: rice.MustFindBox("scripts"),
		timerFunc: newTimer,
		timers: make([]*TimerEntry, 0, TIMERS_CAPACITY),
		callbackIndex: 1,
		ruleMap: make(map[string]*Rule),
		ruleList: make([]string, 0, RULES_CAPACITY),
		notedCells: nil,
		cellToRuleMap: make(map[*Cell][]*Rule),
		rulesWithoutCells: make(map[*Rule]bool),
	}

	engine.initCallbackList("ruleEngineTimers")
	engine.initCallbackList("processes")
	engine.initCallbackList("ruleFuncs")

	engine.ctx.PushGlobalObject()
	engine.defineEngineFunctions(map[string]func() int {
		"defineVirtualDevice": engine.esDefineVirtualDevice,
		"log": engine.esLog,
		"debug": engine.esDebug,
		"publish": engine.esPublish,
		"_wbDevObject": engine.esWbDevObject,
		"_wbCellObject": engine.esWbCellObject,
		"_wbStartTimer": engine.esWbStartTimer,
		"_wbStopTimer": engine.esWbStopTimer,
		"_wbSpawn": engine.esWbSpawn,
		"_wbDefineRule": engine.esWbDefineRule,
		"runRules": engine.esWbRunRules,
	})
	engine.ctx.Pop()
	if err := engine.loadLib(); err != nil {
		wbgo.Error.Panicf("failed to load runtime library: %s", err)
	}
	return
}

func (engine *RuleEngine) initCallbackList(propName string) {
	// callback list stash property holds callback functions referenced by ids
	engine.ctx.PushGlobalStash()
	engine.ctx.PushObject()
	engine.ctx.PutPropString(-2, propName)
	engine.ctx.Pop()
}

func (engine *RuleEngine) pushCallbackKey(key interface{}) {
	switch key.(type) {
	case int:
		engine.ctx.PushNumber(float64(key.(int)))
	case esCallback:
		engine.ctx.PushString(strconv.FormatUint(uint64(key.(esCallback)), 16))
	default:
		log.Panicf("bad callback key: %v", key)
	}
}

func (engine *RuleEngine) invokeCallback(propName string, key interface{}, args objx.Map) bool {
	engine.ctx.PushGlobalStash()
	engine.ctx.GetPropString(-1, propName)
	engine.pushCallbackKey(key)
	argCount := 0
	if args != nil {
		PushJSObject(engine.ctx, args)
		argCount++
	}
	r := false
	if s := engine.ctx.PcallProp(-2 - argCount, argCount); s != 0 {
		wbgo.Error.Printf("failed to invoke callback %s[%v]: %s",
			propName, key, engine.ctx.SafeToString(-1))
	} else {
		r = engine.ctx.ToBoolean(-1)
	}

	engine.ctx.Pop3() // pop: result, callback list object, global stash
	return r
}

// storeCallback stores the callback from the specified stack index
// (which should be >= 0) at 'key' in the callback list specified as propName.
// If key is specified as nil, a new callback key is generated and returned
// as uint64. In this case the returned value is guaranteed to be
// greater than zero.
func (engine *RuleEngine) storeCallback(propName string, callbackStackIndex int, key interface{}) esCallback {
	var r esCallback = 0
	if key == nil {
		r = engine.callbackIndex
		key = r
		engine.callbackIndex++
	}

	engine.ctx.PushGlobalStash()
	engine.ctx.GetPropString(-1, propName)
	engine.pushCallbackKey(key)
	if callbackStackIndex < 0 {
		engine.ctx.Dup(callbackStackIndex - 3)
	} else {
		engine.ctx.Dup(callbackStackIndex)
	}
	engine.ctx.PutProp(-3) // callbackList[key] = callback
	engine.ctx.Pop2()
	return r
}

func (engine *RuleEngine) removeCallback(propName string, key interface{}) {
	engine.ctx.PushGlobalStash()
	engine.ctx.GetPropString(-1, propName)
	engine.pushCallbackKey(key)
	engine.ctx.DelProp(-2)
	engine.ctx.Pop()
}

func (engine *RuleEngine) SetTimerFunc(timerFunc TimerFunc) {
	engine.timerFunc = timerFunc
}

func (engine *RuleEngine) loadLib() error {
	libStr, err := engine.scriptBox.String(LIB_FILE)
	if err != nil {
		return  err
	}
	engine.ctx.PushString(LIB_FILE)
	// we use PcompileStringFilename here to get readable stacktraces
	if r := engine.ctx.PcompileStringFilename(0, libStr); r != 0 {
		defer engine.ctx.Pop()
		return fmt.Errorf("failed to compile lib.js: %s", engine.ctx.SafeToString(-1))
	}
	defer engine.ctx.Pop()
	if r := engine.ctx.Pcall(0); r != 0 {
		return fmt.Errorf("failed to run lib.js: %s", engine.ctx.SafeToString(-1))
	}
	return nil
}

func (engine *RuleEngine) SetLogFunc(logFunc LogFunc) {
	engine.logFunc = logFunc
}

func (engine *RuleEngine) esDefineVirtualDevice() int {
	if engine.ctx.GetTop() != 2 || !engine.ctx.IsString(-2) || !engine.ctx.IsObject(-1) {
		return duktape.DUK_RET_ERROR
	}
	name := engine.ctx.GetString(-2)
	title := name
	obj := GetJSObject(engine.ctx, -1).(objx.Map)
	if obj.Has("title") {
		title = obj.Get("title").Str(name)
	}
	dev := engine.model.EnsureLocalDevice(name, title)
	if obj.Has("cells") {
		if v := obj.Get("cells"); !v.IsMSI() {
			return duktape.DUK_RET_ERROR
		} else {
			for cellName, maybeCellDef := range v.MSI() {
				cellDef, ok := maybeCellDef.(map[string]interface{})
				if !ok {
					return duktape.DUK_RET_ERROR
				}
				cellType, ok := cellDef["type"]
				if !ok {
					return duktape.DUK_RET_ERROR
				}
				cellValue, ok := cellDef["value"]
				if !ok {
					return duktape.DUK_RET_ERROR
				}
				if cellType == "range" {
					fmax := DEFAULT_CELL_MAX
					max, ok := cellDef["max"]
					if ok {
						fmax, ok = max.(float64)
						if !ok {
							return duktape.DUK_RET_ERROR
						}
					}
					// FIXME: can be float
					dev.SetRangeCell(cellName, cellValue, fmax)
				} else {
					dev.SetCell(cellName, cellType.(string), cellValue)
				}
			}
		}
	}
	return 0
}

func (engine *RuleEngine) getLogStrArg() (string, bool) {
	strs := make([]string, 0, 100)
	for n := -engine.ctx.GetTop(); n < 0; n++ {
		strs = append(strs, engine.ctx.SafeToString(n))
	}
	if len(strs) > 0 {
		return strings.Join(strs, " "), true
	}
	return "", false
}

func (engine *RuleEngine) esLog() int {
	s, show := engine.getLogStrArg()
	if show {
		engine.logFunc(s)
	}
	return 0
}

func (engine *RuleEngine) esDebug() int {
	s, show := engine.getLogStrArg()
	if show {
		wbgo.Debug.Printf("[rule debug] %s", s)
	}
	return 0
}

func (engine *RuleEngine) esPublish() int {
	retain := false
	qos := 0
	if engine.ctx.GetTop() == 4 {
		retain = engine.ctx.ToBoolean(-1)
		engine.ctx.Pop()
	}
	if engine.ctx.GetTop() == 3 {
		qos = int(engine.ctx.ToNumber(-1))
		engine.ctx.Pop()
		if qos < 0 || qos > 2 {
			return duktape.DUK_RET_ERROR
		}
	}
	if engine.ctx.GetTop() != 2 {
		return duktape.DUK_RET_ERROR
	}
	if !engine.ctx.IsString(-2) {
		return duktape.DUK_RET_TYPE_ERROR
	}
	engine.mqttClient.Publish(wbgo.MQTTMessage{
		Topic: engine.ctx.GetString(-2),
		Payload: engine.ctx.SafeToString(-1),
		QoS: byte(qos),
		Retained: retain,
	})
	return 0
}

func (engine *RuleEngine) esWbDevObject() int {
	wbgo.Debug.Printf("esWbDevObject(): top=%d isString=%v", engine.ctx.GetTop(), engine.ctx.IsString(-1))
	if engine.ctx.GetTop() != 1 || !engine.ctx.IsString(-1) {
		return duktape.DUK_RET_ERROR
	}
	dev := engine.model.EnsureDevice(engine.ctx.GetString(-1))
	engine.ctx.PushGoObject(dev)
	return 1
}

func (engine *RuleEngine) startTrackingCells() {
	engine.notedCells = make(map[*Cell]bool)
}

func (engine *RuleEngine) storeRuleCell(rule *Rule, cell *Cell) {
	list, found := engine.cellToRuleMap[cell]
	if !found {
		list = make([]*Rule, 0, CELL_RULES_CAPACITY)
	}
	engine.cellToRuleMap[cell] = append(list, rule)
}

func (engine *RuleEngine) storeRuleTrackedCells(rule *Rule) {
	if len(engine.notedCells) > 0 {
		for cell, _ := range engine.notedCells {
			engine.storeRuleCell(rule, cell)
		}
	} else {
		wbgo.Debug.Printf("rule %s doesn't use any cells", rule.name)
		engine.rulesWithoutCells[rule] = true
	}
	engine.notedCells = nil
}

func (engine *RuleEngine) trackCell(cell *Cell) {
	if engine.notedCells != nil {
		engine.notedCells[cell] = true
	}
}

func (engine *RuleEngine) esWbCellObject() int {
	if engine.ctx.GetTop() != 2 || !engine.ctx.IsString(-1) || !engine.ctx.IsObject(-2) {
		return duktape.DUK_RET_ERROR
	}
	dev, ok := engine.ctx.GetGoObject(-2).(CellModelDevice)
	if !ok {
		wbgo.Error.Printf("invalid _wbCellObject call")
		return duktape.DUK_RET_TYPE_ERROR
	}
	cell := dev.EnsureCell(engine.ctx.GetString(-1))
	engine.ctx.PushGoObject(cell)
	engine.defineEngineFunctions(map[string]func() int {
		"rawValue": func () int {
			engine.trackCell(cell)
			engine.ctx.PushString(cell.RawValue())
			return 1
		},
		"value": func () int {
			engine.trackCell(cell)
			m := objx.New(map[string]interface{} {
				"v": cell.Value(),
			})
			PushJSObject(engine.ctx, m)
			return 1
		},
		"setValue": func () int {
			engine.trackCell(cell)
			if engine.ctx.GetTop() != 1 || !engine.ctx.IsObject(-1) {
				return duktape.DUK_RET_ERROR
			}
			m, ok := GetJSObject(engine.ctx, -1).(objx.Map)
			if !ok || !m.Has("v") {
				wbgo.Error.Printf("invalid cell definition")
				return duktape.DUK_RET_TYPE_ERROR
			}
			cell.SetValue(m["v"])
			return 1
		},
		"isComplete": func () int {
			engine.trackCell(cell)
			engine.ctx.PushBoolean(cell.IsComplete())
			return 1
		},
	})
	return 1
}

func (engine *RuleEngine) fireTimer(n int) {
	entry := engine.timers[n - 1]
	if entry == nil {
		wbgo.Error.Printf("firing unknown timer %d", n)
		return
	}
	engine.invokeCallback("ruleEngineTimers", n, nil)

	if !entry.periodic {
		engine.removeTimer(n)
	}
}

func (engine *RuleEngine) esWbStartTimer() int {
	if engine.ctx.GetTop() != 3 || !engine.ctx.IsFunction(-3) || !engine.ctx.IsNumber(-2) {
		return duktape.DUK_RET_ERROR
	}

	ms := engine.ctx.GetNumber(-2)
	periodic := engine.ctx.ToBoolean(-1)

	entry := &TimerEntry{
		//,
		periodic: periodic,
		quit: make(chan struct{}, 2),
	}

	var n int
	for n = 1; n <= len(engine.timers); n++ {
		if engine.timers[n - 1] == nil {
			engine.timers[n - 1] = entry
		}
		break
	}
	if n > len(engine.timers) {
		engine.timers = append(engine.timers, entry)
	}

	engine.storeCallback("ruleEngineTimers", 0, n)

	entry.timer = engine.timerFunc(n, time.Duration(ms * float64(time.Millisecond)), periodic)
	tickCh := entry.timer.GetChannel()
	go func () {
		for {
			select {
			case <- tickCh:
				engine.model.CallSync(func () {
					engine.fireTimer(n)
				})
				if !periodic {
					return
				}
			case <- entry.quit:
				entry.timer.Stop()
				return
			}
		}
	}()

	engine.ctx.PushNumber(float64(n))
	return 1
}

func (engine *RuleEngine) removeTimer(n int) {
	engine.removeCallback("ruleEngineTimers", n)
	engine.timers[n - 1] = nil
}

func (engine *RuleEngine) esWbStopTimer() int {
	if engine.ctx.GetTop() != 1 || !engine.ctx.IsNumber(-1) {
		return duktape.DUK_RET_ERROR
	}
	n := engine.ctx.GetInt(-1)
	if n == 0 {
		wbgo.Error.Printf("timer id cannot be zero")
		return 0
	}
	if entry := engine.timers[n - 1]; entry != nil {
		engine.removeTimer(n)
		close(entry.quit)
	} else {
		wbgo.Error.Printf("trying to stop unknown timer: %d", n)
	}
	return 0
}

func (engine *RuleEngine) esWbSpawn() int {
	if engine.ctx.GetTop() != 5 || !engine.ctx.IsArray(0) || !engine.ctx.IsBoolean(2) ||
		!engine.ctx.IsBoolean(3) {
		return duktape.DUK_RET_ERROR
	}

	args := StringArrayToGo(engine.ctx, 0)
	if len(args) == 0 {
		return duktape.DUK_RET_ERROR
	}

	callbackIndex := NO_CALLBACK

	if engine.ctx.IsFunction(1) {
		callbackIndex = engine.storeCallback("processes", 1, nil)
	} else if !engine.ctx.IsNullOrUndefined(1) {
		return duktape.DUK_RET_ERROR
	}

	var input *string
	if engine.ctx.IsString(4) {
		instr := engine.ctx.GetString(4)
		input = &instr
	} else if !engine.ctx.IsNullOrUndefined(4) {
		return duktape.DUK_RET_ERROR
	}

	captureOutput := engine.ctx.GetBoolean(2)
	captureErrorOutput := engine.ctx.GetBoolean(3)
	command := engine.ctx.GetString(0)
	go func () {
		r, err := Spawn(args[0], args[1:], captureOutput, captureErrorOutput, input)
		if err != nil {
			wbgo.Error.Printf("external command failed: %s", err)
			return
		}
		if callbackIndex > 0 {
			engine.model.CallSync(func () {
				args := objx.New(map[string]interface{} {
					"exitStatus": r.ExitStatus,
				})
				if captureOutput {
					args["capturedOutput"] = r.CapturedOutput
				}
				args["capturedErrorOutput"] = r.CapturedErrorOutput
				engine.invokeCallback("processes", callbackIndex, args)
				engine.removeCallback("processes", callbackIndex)
			})
		} else if r.ExitStatus != 0 {
			wbgo.Error.Printf("command '%s' failed: %s", command, err)
		}
	}()
	return 0
}

func (engine *RuleEngine) esWbDefineRule() int {
	if engine.ctx.GetTop() != 2 || !engine.ctx.IsString(0) || !engine.ctx.IsObject(1) {
		engine.logFunc(fmt.Sprintf("bad rule definition"))
		return duktape.DUK_RET_ERROR
	}
	name := engine.ctx.GetString(0)
	newRule, err := newRule(engine, name, 1)
	if err != nil {
		// FIXME: proper error handling
		engine.logFunc(fmt.Sprintf("bad definition of rule '%s': %s", name, err))
		return duktape.DUK_RET_ERROR
	}
	if oldRule, found := engine.ruleMap[name]; found {
		oldRule.Destroy()
	} else {
		engine.ruleList = append(engine.ruleList, name)
	}
	engine.ruleMap[name] = newRule
	return 0
}

func (engine *RuleEngine) esWbRunRules() int {
	switch engine.ctx.GetTop() {
	case 0:
		engine.RunRules(nil)
	case 2:
		devName := engine.ctx.SafeToString(0)
		cellName := engine.ctx.SafeToString(1)
		engine.RunRules(&CellSpec{devName, cellName})
	default:
		return duktape.DUK_RET_ERROR
	}
	return 0
}

func (engine *RuleEngine) defineEngineFunctions(fns map[string]func() int) {
	for name, fn := range fns {
		f := fn
		engine.ctx.PushGoFunc(func (*duktape.Context) int {
			return f()
		})
		engine.ctx.PutPropString(-2, name)
	}
}

func (engine *RuleEngine) getCell(devName, cellName string) *Cell {
	return engine.model.EnsureDevice(devName).EnsureCell(cellName)
}

func (engine *RuleEngine) RunRules(cellSpec *CellSpec) {
	var cell *Cell
	if cellSpec != nil {
		cell = engine.getCell(cellSpec.DevName, cellSpec.CellName)
		if cell.IsComplete() {
			// cell-dependent rules aren't run when any of their
			// condition cells are incomplete
			if list, found := engine.cellToRuleMap[cell]; found {
				for _, rule := range list {
					rule.ShouldCheck()
				}
			}
		}
		for rule, _ := range engine.rulesWithoutCells {
			rule.ShouldCheck()
		}
	}

	for _, name := range engine.ruleList {
		engine.ruleMap[name].Check(cell)
	}
}

func (engine *RuleEngine) LoadScript(path string) error {
	defer engine.ctx.Pop()
	if r := engine.ctx.PevalFile(path); r != 0 {
		return fmt.Errorf("failed to load %s: %s", path, engine.ctx.SafeToString(-1))
	}
	return nil
}

func (engine *RuleEngine) Start() {
	if engine.cellChange != nil {
		return
	}
	engine.cellChange = engine.model.AcquireCellChangeChannel()
	ready := make(chan struct{})
	engine.model.WhenReady(func () {
		engine.RunRules(nil)
		close(ready)
	})
	go func () {
		_, _ = <- ready
		for {
			select {
			case cellSpec, ok := <- engine.cellChange:
				if ok {
					if cellSpec != nil {
						wbgo.Debug.Printf(
							"rule engine: running rules after cell change: %s/%s",
							cellSpec.DevName, cellSpec.CellName)
					} else {
						wbgo.Debug.Printf(
							"rule engine: running rules")
					}
					engine.model.CallSync(func () {
						engine.RunRules(cellSpec)
					})
				} else {
					wbgo.Debug.Printf("engine stopped")
					for _, entry := range engine.timers {
						if entry != nil {
							close(entry.quit)
						}
					}
					engine.timers = engine.timers[:0]
					engine.model.ReleaseCellChangeChannel(engine.cellChange)
					engine.cellChange = nil
				}
			}
		}
	}()
}
