package config

import (
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/spf13/viper"
)

// SettingDoc is one startup setting as docs/config.json publishes it: the
// variable and key that set it, its type and default, and what it does.
//
// Read from Config itself — the mapstructure key, the desc tag beside it, the
// default Load starts from and the variable Load binds — so the reference
// cannot name a setting Pando does not read, or miss one it does (R-271).
type SettingDoc struct {
	Env         string `json:"env"`
	Key         string `json:"key"`
	Type        string `json:"type"`
	Default     string `json:"default"`
	Description string `json:"description"`
	Group       string `json:"group"`
	Secret      bool   `json:"secret"`
}

// Reference is every setting in Config, sorted by key.
func Reference() []SettingDoc {
	v := viper.New()
	setDefaults(v)

	var out []SettingDoc
	root := reflect.TypeOf(Config{})
	for i := range root.NumField() {
		section := root.Field(i)
		group := section.Tag.Get("mapstructure")
		if group == "" || group == "-" || section.Type.Kind() != reflect.Struct {
			continue
		}
		for j := range section.Type.NumField() {
			f := section.Type.Field(j)
			name := f.Tag.Get("mapstructure")
			if name == "" || name == "-" {
				continue
			}
			key := group + "." + name
			out = append(out, SettingDoc{
				Env:         envName(key),
				Key:         key,
				Type:        typeName(f.Type),
				Default:     defaultOf(v.Get(key)),
				Description: f.Tag.Get("desc"),
				Group:       group,
				Secret:      secret[key],
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

var durationType = reflect.TypeOf(time.Duration(0))

// typeName is how a setting's value is written: a duration is "15s" or "5m",
// not a number.
func typeName(t reflect.Type) string {
	if t == durationType {
		return "duration"
	}
	switch t.Kind() {
	case reflect.Bool:
		return "bool"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return "int"
	}
	return t.Kind().String()
}

// defaultOf renders a default as a variable would be set to it. A key with no
// default at all is empty.
func defaultOf(value any) string {
	switch d := value.(type) {
	case nil:
		return ""
	case time.Duration:
		return d.String()
	}
	return fmt.Sprint(value)
}
