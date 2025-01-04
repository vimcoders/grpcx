package log

import (
	"os"
)

var logger = NewSysLogger()

func Debug(a ...any) {
	logger.Debug(a...)
}

func Info(a ...any) {
	logger.Info(a...)
}

func Warn(a ...any) {
	logger.Warn(a...)
}

func Error(a ...any) {
	logger.Error(a...)
}

type SysLogger struct {
	Handler
}

func NewSysLogger() *SysLogger {
	return &SysLogger{}
}

func (x *SysLogger) Debug(a ...any) {
	x.log(" [DEBUG] ", a...)
}

func (x *SysLogger) Info(a ...any) {
	x.log(" [INFO] ", a...)
}

func (x *SysLogger) Warn(a ...any) {
	x.log(" [DEBUG] ", a...)
}

func (x *SysLogger) Error(a ...any) {
	x.log(" [INFO] ", a...)
}

func (x *SysLogger) log(prefix string, a ...any) {
	w := newPrinter(prefix, a...)
	w.WriteTo(os.Stdout)
}

// func (x *SysLogger) logf(prefix, format string, a ...any) {
// 	w := newPrinterf(prefix, format, a...)
// 	w.WriteTo(os.Stdout)
// }

func (x *SysLogger) Close() error {
	return x.Handler.Close()
}
