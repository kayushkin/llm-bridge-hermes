module github.com/kayushkin/llm-bridge-hermes

go 1.25.0

require (
	github.com/kayushkin/aiauth v0.0.0
	github.com/kayushkin/llm-bridge v0.0.0
)

require (
	github.com/anthropics/anthropic-sdk-go v1.37.0 // indirect
	github.com/tidwall/gjson v1.18.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	golang.org/x/sync v0.16.0 // indirect
)

replace github.com/kayushkin/llm-bridge => ../llm-bridge

replace github.com/kayushkin/aiauth => ../aiauth
