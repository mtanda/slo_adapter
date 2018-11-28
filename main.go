package main

import (
	"context"
	"flag"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/golang/protobuf/proto"
	"github.com/golang/snappy"
	config_util "github.com/prometheus/common/config"
	"github.com/prometheus/common/model"
	"github.com/prometheus/common/promlog"
	"github.com/prometheus/prometheus/prompb"
	"github.com/prometheus/prometheus/storage/remote"

	yaml "gopkg.in/yaml.v2"
)

type cliConfig struct {
	listenAddr    string
	configFile    string
	remoteReadUrl string
}

type Config struct {
	Pattern string  `yaml:"pattern"`
	Value   float64 `yaml:"value"`
}
type Configs struct {
	Configs []Config `yaml:"slo_configs"`
}

func runQuery(req prompb.ReadRequest, client *remote.Client, s Configs, logger log.Logger) ([]*prompb.TimeSeries, error) {
	for _, m := range req.Queries[0].Matchers {
		if m.Name == "__name__" {
			if strings.Index(m.Value, "slo:") == 0 {
				m.Value = m.Value[len("slo:"):]
			} else {
				return make([]*prompb.TimeSeries, 0), nil
			}
		}
	}

	result, err := client.Read(context.Background(), req.Queries[0])
	if err != nil {
		level.Error(logger).Log("err", err)
		return make([]*prompb.TimeSeries, 0), nil
	}

	for _, ts := range result.Timeseries {
		name := ""
		for _, label := range ts.Labels {
			switch label.Name {
			case "__name__":
				name = label.Value
				label.Value = "slo:" + label.Value
			}
		}
		idx := -1
		for i := range s.Configs {
			if name == s.Configs[i].Pattern {
				idx = i
			}
		}
		if idx != -1 {
			for _, sample := range ts.Samples {
				sample.Value = float64(s.Configs[idx].Value)
			}
		}
	}

	return result.Timeseries, nil
}

func main() {
	var cfg cliConfig
	flag.StringVar(&cfg.listenAddr, "web.listen-address", ":9416", "Address to listen on for web endpoints.")
	flag.StringVar(&cfg.configFile, "config.file", "./configs.yml", "Configuration file path.")
	flag.StringVar(&cfg.remoteReadUrl, "remote-read.url", "http://localhost:9090/api/v1/read", "Address to read.")
	flag.Parse()

	logLevel := promlog.AllowedLevel{}
	logLevel.Set("info")
	logger := promlog.New(logLevel)

	buf, err := ioutil.ReadFile(cfg.configFile)
	if err != nil {
		level.Error(logger).Log("err", err)
		panic(err)
	}

	var s Configs
	err = yaml.Unmarshal(buf, &s)
	if err != nil {
		level.Error(logger).Log("err", err)
		panic(err)
	}

	u, err := url.Parse(cfg.remoteReadUrl)
	if err != nil {
		level.Error(logger).Log("err", err)
		panic(err)
	}
	client, err := remote.NewClient(1, &remote.ClientConfig{
		URL:     &config_util.URL{URL: u},
		Timeout: model.Duration(60 * time.Second),
	})
	if err != nil {
		level.Error(logger).Log("err", err)
		panic(err)
	}

	srv := &http.Server{Addr: cfg.listenAddr}
	http.HandleFunc("/read", func(w http.ResponseWriter, r *http.Request) {
		compressed, err := ioutil.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		reqBuf, err := snappy.Decode(nil, compressed)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		var req prompb.ReadRequest
		if err := proto.Unmarshal(reqBuf, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if len(req.Queries) != 1 {
			http.Error(w, "Can only handle one query.", http.StatusBadRequest)
			return
		}

		result, err := runQuery(req, client, s, logger)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		resp := prompb.ReadResponse{
			Results: []*prompb.QueryResult{
				{
					Timeseries: result,
				},
			},
		}

		data, err := proto.Marshal(&resp)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/x-protobuf")
		if _, err := w.Write(snappy.Encode(nil, data)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	})

	level.Info(logger).Log("msg", "Listening on "+cfg.listenAddr)
	if err := srv.ListenAndServe(); err != nil {
		level.Error(logger).Log("err", err)
		panic(err)
	}
}
