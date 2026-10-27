package config

import (
	"fmt"
	"time"

	"github.com/spf13/viper"
)

// Cluster is the cluster's timing (R-421, R-420): how long a host may reach
// too few of the others before it fences, how often hosts probe each other,
// and how often the standby pass runs and ships an app's volumes. The
// defaults are the requirement's [P] values; every host of a cluster must be
// given the same ones, since one host's T is what the others count from.
type Cluster struct {
	LivenessPeriod   time.Duration `mapstructure:"liveness_period" desc:"T: how long a host may reach fewer than half the cluster before it stops its apps. Its standbys start after 3T. Every host of the cluster must have the same value. From 6s to 10m."`
	LivenessInterval time.Duration `mapstructure:"liveness_interval" desc:"How often a host probes every other. 0 means a sixth of liveness_period, which is also the most it may be."`
	StandbyEvery     time.Duration `mapstructure:"standby_every" desc:"How often the standby pass looks at every app: places, refreshes and ships its standby. From 5s to an hour."`
	ShipEvery        time.Duration `mapstructure:"ship_every" desc:"How old an app's newest volume copy on its standby may get before another is shipped. At least standby_every, and at most a day."`
}

// The shipped cluster timings, which internal/core/liveness (DefaultPeriod)
// and internal/core/standby (DefaultEvery, DefaultShipEvery) hold too.
const (
	DefaultLivenessPeriod = 30 * time.Second
	DefaultStandbyEvery   = time.Minute
	DefaultShipEvery      = 5 * time.Minute
)

// The bounds Cluster's timings are held to.
const (
	minLivenessPeriod   = 6 * time.Second
	maxLivenessPeriod   = 10 * time.Minute
	minLivenessInterval = 500 * time.Millisecond
	minStandbyEvery     = 5 * time.Second
	maxStandbyEvery     = time.Hour
	maxShipEvery        = 24 * time.Hour
)

func setClusterDefaults(v *viper.Viper) {
	v.SetDefault("cluster.liveness_period", DefaultLivenessPeriod)
	v.SetDefault("cluster.liveness_interval", time.Duration(0))
	v.SetDefault("cluster.standby_every", DefaultStandbyEvery)
	v.SetDefault("cluster.ship_every", DefaultShipEvery)
}

// LoadCluster reads only the cluster's timing, from the environment and the
// config file at path when there is one: for the host agent, which needs no
// database and so does not Load.
func LoadCluster(path string) (Cluster, error) {
	v, err := newViper(path)
	if err != nil {
		return Cluster{}, err
	}
	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return Cluster{}, fmt.Errorf("parsing configuration: %w", err)
	}
	return cfg.Cluster, cfg.Cluster.validate()
}

func (c Cluster) validate() error {
	if c.LivenessPeriod < minLivenessPeriod || c.LivenessPeriod > maxLivenessPeriod {
		return fmt.Errorf("PANDO_CLUSTER_LIVENESS_PERIOD is %s. Valid answer: a duration from %s to %s, such as %s, the same on every host",
			c.LivenessPeriod, minLivenessPeriod, maxLivenessPeriod, DefaultLivenessPeriod)
	}
	if most := c.LivenessPeriod / 6; c.LivenessInterval != 0 && (c.LivenessInterval < minLivenessInterval || c.LivenessInterval > most) {
		return fmt.Errorf("PANDO_CLUSTER_LIVENESS_INTERVAL is %s. Valid answer: 0, for a sixth of the liveness period, or a duration from %s to %s, a sixth of the liveness period",
			c.LivenessInterval, minLivenessInterval, most)
	}
	if c.StandbyEvery < minStandbyEvery || c.StandbyEvery > maxStandbyEvery {
		return fmt.Errorf("PANDO_CLUSTER_STANDBY_EVERY is %s. Valid answer: a duration from %s to %s, such as %s",
			c.StandbyEvery, minStandbyEvery, maxStandbyEvery, DefaultStandbyEvery)
	}
	if c.ShipEvery < c.StandbyEvery || c.ShipEvery > maxShipEvery {
		return fmt.Errorf("PANDO_CLUSTER_SHIP_EVERY is %s. Valid answer: a duration from the standby pass's interval, %s, to %s, such as %s",
			c.ShipEvery, c.StandbyEvery, maxShipEvery, DefaultShipEvery)
	}
	return nil
}
