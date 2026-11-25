package main

// Shared literals of the steering and transcript tests.
const (
	lampOnJSON      = `{"state":"on"}`
	lampDesc        = "turns a light on"
	stopCommand     = "/stop"
	wireOllama      = "ollama"
	kindsToolAnswer = "user,turn_context,assistant,tool,assistant"
	kindsStopped    = "user,turn_context,assistant,tool,turn_stopped"
	metaTransport   = "transport"
	blockKindText   = "text"
	blockKindUse    = "tool_use"
	blockKindResult = "tool_result"
	turnWorkText    = "turn"
	oneText         = "one"
	nextOneText     = "next one"
	fmtSkillName    = "fmt-skill"
	testConn        = "c1"
)
