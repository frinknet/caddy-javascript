package vm

import (
	quickjs "github.com/go-quickjs/go-quickjs"
)

// channelScopeSource renames BroadcastChannel under a per-VM prefix. The
// engine's channels are process-wide by name, so without a prefix two site VMs
// of the same process, such as an old and a new generation, would exchange
// messages. The script still reads its own name.
const channelScopeSource = `(function (prefix) {
	const Original = globalThis.BroadcastChannel;
	if (typeof Original !== "function") return;
	const names = new WeakMap();
	class BroadcastChannel extends Original {
		constructor(name, options) {
			const plain = String(name);
			super(prefix + plain, options);
			names.set(this, plain);
		}
		get name() { return names.get(this); }
	}
	globalThis.BroadcastChannel = BroadcastChannel;
})`

// installChannelScope scopes a runtime's broadcast names to its VM. A worker's
// runtime is installed with the same prefix, so it talks to its parent but not
// to another VM.
func installChannelScope(rt *quickjs.Runtime, prefix string) error {
	install, err := rt.Eval(channelScopeSource)
	if err != nil {
		return err
	}
	_, err = install.Call(prefix)
	return err
}
