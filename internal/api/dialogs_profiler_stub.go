//go:build !performance

package api

type getDialogsProfiler struct{}

func startGetDialogsProfiler() *getDialogsProfiler { return nil }
func (*getDialogsProfiler) mark(string)            {}
func (*getDialogsProfiler) finish()                {}
