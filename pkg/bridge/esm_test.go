package bridge

import "testing"

func TestESMImportLocalModule(t *testing.T) {
	p, dir := newModuleProgram(t)
	writeModule(t, dir, "lib.mjs", `
		export const x = 1;
		export default function() { return 2; }
	`)
	if err := p.Load([]byte(`
		import double, { x } from "./lib.mjs";
		globalThis.out = x + double();
	`)); err != nil {
		t.Fatal(err)
	}
	if got := evalString(t, p, "out"); got != "3" {
		t.Fatalf("out = %q", got)
	}
}

func TestESMImportCommonJSModule(t *testing.T) {
	p, dir := newModuleProgram(t)
	writeModule(t, dir, "lib.js", `module.exports = { x: 5 }`)
	if err := p.Load([]byte(`
		import { x } from "./lib.js";
		globalThis.out = x;
	`)); err != nil {
		t.Fatal(err)
	}
	if got := evalString(t, p, "out"); got != "5" {
		t.Fatalf("out = %q", got)
	}
}

func TestESMImportBuiltin(t *testing.T) {
	p, _ := newModuleProgram(t)
	if err := p.Load([]byte(`
		import path from "path";
		globalThis.out = path.join("a", "b");
	`)); err != nil {
		t.Fatal(err)
	}
	if got := evalString(t, p, "out"); got != "a/b" {
		t.Fatalf("out = %q", got)
	}
}
