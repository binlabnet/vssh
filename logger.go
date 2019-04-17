package main

import (
	"fmt"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func Logger(level string) (*zap.SugaredLogger, error) {
	zcfg := zap.NewDevelopmentConfig()
	zcfg.DisableCaller = true
	zcfg.DisableStacktrace = true
	zcfg.Sampling = nil
	zcfg.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	//zcfg := zap.NewProductionConfig()
	//zcfg.Encoding = "console"
	//zcfg := zap.NewDevelopmentConfig()
	loglevel := zapcore.DebugLevel
	_ = loglevel.Set(level)
	zcfg.Level.SetLevel(loglevel)
	zcfg.Sampling = nil
	l, err := zcfg.Build()
	if err != nil {
		return nil, fmt.Errorf("unable to initialize zap logger: %s", err)
	}
	return l.Sugar(), nil
}
