package main

import "strings"

// withArchitectureCapability — runner этой версии умеет подкоманду
// architecture и сам объявляет capability architecture (add-architecture-c4):
// задачи описания архитектуры и шаг проверки описания идут только к таким
// runner'ам. Runner выкатки (только deploy) её не получает.
func withArchitectureCapability(caps []string) []string {
	out := make([]string, 0, len(caps)+1)
	has, onlyDeploy := false, true
	for _, c := range caps {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if c == "architecture" {
			has = true
		}
		if c != "deploy" && c != "docker" {
			onlyDeploy = false
		}
		out = append(out, c)
	}
	if len(out) == 1 && out[0] == "load" {
		return out
	}
	if !has && !onlyDeploy {
		out = append(out, "architecture")
	}
	return out
}

// deliveryOnly исключает агентский цикл у детерминированного Docker-driver.
func deliveryOnly(caps []string) bool {
	has := false
	for _, c := range caps {
		switch strings.TrimSpace(c) {
		case "deploy":
			has = true
		case "docker", "":
		default:
			return false
		}
	}
	return has
}
