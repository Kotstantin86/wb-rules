// -*- mode: js2-mode -*-

defineVirtualDevice("stabSettings", {
  title: "Stabilization Settings",
  cells: {
    enabled: {
      type: "switch",
      value: false
    },
    lowThreshold: {
      type: "range",
      max: 40,
      value: 20
    },
    highThreshold: {
      type: "range",
      max: 50,
      value: 22
    }
  }
});

defineRule("heaterOn", {
  asSoonAs: function () {
    return dev.stabSettings.enabled && dev.Weather["Temp 1"] < dev.stabSettings.lowThreshold;
  },
  then: function () {
    log("heaterOn fired");
    dev.Relays["Relay 1"] = true;
    startTicker("heating", 3000);
  }
});

defineRule("heaterOff", {
  when: function () {
    return dev.Relays["Relay 1"] &&
      (!dev.stabSettings.enabled || dev.Weather["Temp 1"] >= dev.stabSettings.highThreshold);
  },
  then: function () {
    log("heaterOff fired");
    dev.Relays["Relay 1"] = false;
    timers.heating.stop();
    startTimer("heatingOff", 1000);
  }
});

defineRule("ht", {
  when: function () {
    return timers.heating.firing;
  },
  then: function () {
    log("heating timer fired");
  }
});

defineRule("htoff", {
  when: function () {
    return timers.heatingOff.firing;
  },
  then: function () {
    log("heating-off timer fired");
  }
});

defineRule("tempChange", {
  onCellChange: ["Weather/Temp 1", "Weather/Temp 2"],
  then: function (devName, cellName, newValue) {
    log(devName + "/" + cellName + " = " + newValue);
  }
});

defineRule("pressureChange", {
  onCellChange: "Weather/Pressure",
  then: function (devName, cellName, newValue) {
    log("pressure = " + newValue);
    runShellCommand(
      "echo -n 'sampleerr' 1>&2; echo -n " + devName + "/" + cellName + "=" + newValue, {
        captureOutput: true,
        captureErrorOutput: true,
        exitCallback: function (exitCode, capturedOutput, capturedErrorOutput) {
          log("cmd exit code: " + exitCode);
          log("cmd output: " + capturedOutput);
          log("cmd error ouput: " + capturedErrorOutput);
        }
      });
  }
});
