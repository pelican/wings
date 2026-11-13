package parser

import (
	jsonv2 "encoding/json/v2"
	"testing"

	goccy "github.com/goccy/go-json"
)

// benchEggConfigs is shaped like a Minecraft egg: 4 files with 8 replacements
// each, decoded the way remote/http.go decodes the Panel's response.
const benchEggConfigs = `[
{"file":"server.properties","parser":"properties","replace":[{"match":"server-ip","replace_with":"0.0.0.0"},{"match":"server-port","replace_with":"{{server.build.default.port}}"},{"match":"query.port","replace_with":"{{server.build.default.port}}"},{"match":"enable-query","replace_with":"true"},{"match":"online-mode","replace_with":true},{"match":"max-players","replace_with":20},{"match":"motd","replace_with":"\u00a7aA Minecraft Server"},{"match":"level-name","if_value":"regex:^$","replace_with":"world"}]},
{"file":"config.yml","parser":"yaml","replace":[{"match":"listeners[0].query_port","replace_with":"{{server.build.default.port}}"},{"match":"listeners[0].host","replace_with":"0.0.0.0:{{server.build.default.port}}"},{"match":"servers.*.address","replace_with":{"regex:^(localhost)$":"{{config.docker.interface}}"}},{"match":"ip_forward","replace_with":true},{"match":"online_mode","replace_with":true},{"match":"player_limit","replace_with":-1},{"match":"log_commands","replace_with":false},{"match":"timeout","replace_with":30000}]},
{"file":"spigot.yml","parser":"yaml","create_file":false,"replace":[{"match":"settings.bungeecord","replace_with":false},{"match":"settings.restart-on-crash","replace_with":false},{"match":"settings.timeout-time","replace_with":60},{"match":"settings.save-user-cache-on-stop-only","replace_with":false},{"match":"settings.netty-threads","replace_with":4},{"match":"settings.log-villager-deaths","replace_with":true},{"match":"settings.log-named-deaths","replace_with":true},{"match":"messages.whitelist","replace_with":"You are not whitelisted on this server!"}]},
{"file":"bukkit.yml","parser":"yaml","replace":[{"match":"settings.allow-end","value":true},{"match":"settings.warn-on-overload","value":true},{"match":"settings.connection-throttle","value":4000},{"match":"spawn-limits.monsters","value":70},{"match":"spawn-limits.animals","value":10},{"match":"ticks-per.animal-spawns","value":400},{"match":"ticks-per.monster-spawns","value":1},{"match":"aliases","value":"now-in-commands.yml"}]}
]`

func BenchmarkEggConfigParse(b *testing.B) {
	data := []byte(benchEggConfigs)
	run := func(name string, unmarshal func([]byte, any) error) {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				var files []ConfigurationFile
				if err := unmarshal(data, &files); err != nil {
					b.Fatal(err)
				}
				if len(files) != 4 || len(files[3].Replace) != 8 {
					b.Fatalf("bad decode: %d files", len(files))
				}
			}
		})
	}
	// goccy is what remote/http.go uses today.
	run("goccy", goccy.Unmarshal)
	run("json/v2", func(d []byte, v any) error { return jsonv2.Unmarshal(d, v, legacyDecodeOptions) })
}
