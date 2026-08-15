package config

// Load 从环境变量解析并校验出运行时配置，任意必填项缺失或取值非法都会返回错误。
func Load() (Config, error) {
	cfg, err := loadRuntimeConfig()
	if err != nil {
		return Config{}, err
	}

	if err := validateConfig(cfg); err != nil {
		return Config{}, err
	}

	return cfg, nil
}
