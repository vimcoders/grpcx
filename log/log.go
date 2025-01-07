package log

import (
	"os"
	"time"
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
	var buffer buffer
	buffer.Write(time.Now().Format("2006-01-02 15:04:05"))
	buffer.Write(prefix)
	buffer.Appendln(a...)
	buffer.WriteTo(os.Stdout)
}

func (x *SysLogger) Close() error {
	return nil
}
