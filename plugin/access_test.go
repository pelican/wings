package plugin

import "testing"

func TestPanelAccessLevels(t *testing.T) {
	want := map[PanelAccess][]PanelAction{
		PanelAccessOff:    nil,
		PanelAccessView:   {ActionList, ActionToggle, ActionSettings},
		PanelAccessSigned: {ActionList, ActionToggle, ActionSettings, ActionInstallSigned},
		PanelAccessFull:   {ActionList, ActionToggle, ActionSettings, ActionInstallSigned, ActionInstallAny},
	}
	all := []PanelAction{ActionList, ActionToggle, ActionSettings, ActionInstallSigned, ActionInstallAny}
	for level, allowed := range want {
		ok := map[PanelAction]bool{}
		for _, a := range allowed {
			ok[a] = true
		}
		for _, a := range all {
			if level.Allows(a) != ok[a] {
				t.Errorf("%s.Allows(%s) = %v, want %v", level, a, level.Allows(a), ok[a])
			}
		}
	}
}

func TestCurrentPanelAccess(t *testing.T) {
	c := setConfig(t)
	if got := CurrentPanelAccess(); got != PanelAccessView {
		t.Fatalf("default = %q, want view", got)
	}
	p := c.Plugins
	p.PanelAccess = "signed"
	if got := CurrentPanelAccess(); got != PanelAccessView {
		t.Fatalf("a change to a copy must not apply; got %q", got)
	}
	ApplyPolicy(p)
	if got := CurrentPanelAccess(); got != PanelAccessSigned {
		t.Fatalf("after ApplyPolicy = %q, want signed", got)
	}
	t.Setenv(PanelAccessEnv, "full")
	if got := CurrentPanelAccess(); got != PanelAccessFull {
		t.Fatalf("with %s=full = %q", PanelAccessEnv, got)
	}
	t.Setenv(PanelAccessEnv, "everything")
	if got := CurrentPanelAccess(); got != PanelAccessOff {
		t.Fatalf("an unknown level must mean off, got %q", got)
	}
}

// WINGS_PLUGIN_PANEL_ACCESS set to "" is an override like any other value: it
// must not fall back to the default "view" when config.yml says "off".
func TestEmptyPanelAccessEnvFailsClosed(t *testing.T) {
	c := setConfig(t)
	p := c.Plugins
	p.PanelAccess = "off"
	ApplyPolicy(p)
	t.Setenv(PanelAccessEnv, "")
	if got := CurrentPanelAccess(); got != PanelAccessOff {
		t.Fatalf("empty %s over panel_access: off = %q, want off", PanelAccessEnv, got)
	}
	p.PanelAccess = ""
	ApplyPolicy(p)
	if got := CurrentPanelAccess(); got != PanelAccessOff {
		t.Fatalf("empty %s with no panel_access = %q, want off", PanelAccessEnv, got)
	}
}
