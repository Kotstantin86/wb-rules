package wbrules

import (
	"github.com/contactless/wbgo"
	"testing"
)

type LogSuite struct {
	RuleSuiteBase
}

func (s *LogSuite) SetupTest() {
	s.SetupSkippingDefs("testrules_log.js")
}

func (s *LogSuite) TestLog() {
	s.engine.EvalScript("testLog()")
	s.Verify(
		"[info] log()",
		"[info] log.info(42)",
		"[warning] log.warning(42)",
		"[error] log.error(42)",
	)
	s.publish("/devices/wbrules/controls/Rule debugging/on", "1", "wbrules/Rule debugging")
	s.Verify(
		"tst -> /devices/wbrules/controls/Rule debugging/on: [1] (QoS 1)",
		"driver -> /devices/wbrules/controls/Rule debugging: [1] (QoS 1, retained)",
	)
	s.engine.EvalScript("testLog()")
	s.Verify(
		"[info] log()",
		"[debug] debug()",
		"[debug] log.debug(42)",
		"[info] log.info(42)",
		"[warning] log.warning(42)",
		"[error] log.error(42)",
	)
}

func TestLogSuite(t *testing.T) {
	wbgo.RunSuites(t,
		new(LogSuite),
	)
}