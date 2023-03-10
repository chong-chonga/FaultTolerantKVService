package conf

import (
	"errors"
	"gopkg.in/yaml.v3"
)

type KVServiceConf struct {
	Me       int
	KVServer KVServerConf `yaml:"kvserver"`
	Raft     RaftConf     `yaml:"raft"`
}

type KVServerConf struct {
	Password       string `yaml:"password"`
	Port           int    `yaml:"port"`
	MaxRaftState   int    `yaml:"maxRaftState"`
	SessionTimeout int    `yaml:"sessionTimeout"`
	LogEnabled     bool   `yaml:"logEnabled"`
}

type RaftConf struct {
	Port            int      `yaml:"port"`
	ServerAddresses []string `yaml:"server-addresses,flow"`
	RandomInterval  int      `yaml:"randomInterval"`
	ElectionTimeout int      `yaml:"electionTimeout"`
	Log             struct {
		RequestVoteEnabled     bool `yaml:"requestVoteEnabled"`
		AppendEntryEnabled     bool `yaml:"appendEntryEnabled"`
		InstallSnapshotEnabled bool `yaml:"installSnapshotEnabled"`
		PersistEnabled         bool `yaml:"persistEnabled"`
	}
}

func ReadConf(config []byte) (*KVServiceConf, error) {
	if nil == config || len(config) == 0 {
		return nil, errors.New("configuration is empty")
	}
	conf := &KVServiceConf{}
	// If one or more values cannot be decoded due to a type mismatches,
	// decoding continues partially until the end of the YAML content,
	// and a *yaml.TypeError is returned with details for all missed values.
	err := yaml.Unmarshal(config, conf)
	if err != nil {
		return nil, err
	}
	return conf, nil
}
