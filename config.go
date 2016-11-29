package main

import (
	"time"
)

type Config struct {
	ListenPort        int      `yaml:"listenPort"`
	PrometheusPort    int      `yaml:"prometheusPort"`
	RedisTTLDays      int      `yaml:"redisTTLdays"`
	RedisErrorTTLMins int      `yaml:"redisErrorTTLmins"`
	HTTPGetTimeoutSec int      `yaml:"httpGetTimeoutsec"`
	MaxRedirect       int      `yaml:"maxRedirect"`
	MaxImgURL         int      `yaml:"maxImgURL"`
	DescMaxWords      int      `yaml:"descMaxWords"`
	DescMaxChars      int      `yaml:"descMaxChars"`
	ProviderNamesFile string   `yaml:"providerNamesFile"`
	MultiTags         []string `yaml:"multiTags"`
	KeywordsTags      []string `yaml:"keywordsTags"`
	Blacklist         []string `yaml:"blacklist"`
	RedisHost         string   `yaml:"redisHost"`
	RedisDB           int      `yaml:"redisDB"`

	RedisTTL       time.Duration
	RedisErrorTTL  time.Duration
	HTTPGetTimeout time.Duration

	MultiTagsMap map[string]bool
}
