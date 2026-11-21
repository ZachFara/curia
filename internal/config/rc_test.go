package config

import "testing"

func TestRCLaunchDefaultsOffAndNoRepositoryFileTurnsItOn(t *testing.T) {
	if Default().RC.Launch {
		t.Fatal("rc.launch is on by default")
	}
	c, r, err := Resolve(layerRepo(t, "", "[rc]\nlaunch = true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.RC.Launch || !refused(r, "rc.launch") {
		t.Errorf("a repository file turned Remote Control on at launch (on=%v, refused=%+v)", c.RC.Launch, r.Refused)
	}
	c, _, err = Resolve(layerRepo(t, "[rc]\nlaunch = true\n", ""))
	if err != nil || !c.RC.Launch {
		t.Errorf("the user file cannot turn it on: on=%v err=%v", c.RC.Launch, err)
	}
}
