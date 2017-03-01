package common

import "sync"

type config struct {
	SrsRTMPPort  string
	SrsHTTPPort  string
	LpmsRTMPPort string
	LpmsHTTPPort string
}

var instance *config
var once sync.Once

func GetConfig() *config {
	once.Do(func() {
		instance = &config{}
	})
	return instance
}

func SetConfig(srsRTMPPort string, srsHTTPPort string, lpmsRTMPPOrt string, lpmsHTTPPort string) {
	c := GetConfig()
	c.LpmsHTTPPort = lpmsHTTPPort
	c.LpmsRTMPPort = lpmsRTMPPOrt
	c.SrsHTTPPort = srsHTTPPort
	c.SrsRTMPPort = srsRTMPPort
}

// func (self *Config) GetSrsRTMPPort() string {
// 	return self.SrsRTMPPort
// }
