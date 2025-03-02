// Copyright 2020 The Kube-burner Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package prometheus

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"math"
	"net/http"
	"text/template"
	"time"

	"github.com/cloud-bulldozer/kube-burner/log"
	"github.com/cloud-bulldozer/kube-burner/pkg/config"
	"github.com/cloud-bulldozer/kube-burner/pkg/indexers"
	"github.com/cloud-bulldozer/kube-burner/pkg/util"
	api "github.com/prometheus/client_golang/api"
	apiv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	"gopkg.in/yaml.v3"
)

func (bat authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if bat.username != "" {
		req.SetBasicAuth(bat.username, bat.password)
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", bat.token))
	return bat.Transport.RoundTrip(req)
}

// NewPrometheusClient creates a prometheus struct instance with the given parameters
func NewPrometheusClient(configSpec config.Spec, url, token, username, password, uuid string, tlsVerify bool, step time.Duration, metadata map[string]interface{}) (*Prometheus, error) {
	p := Prometheus{
		Step:       step,
		UUID:       uuid,
		ConfigSpec: configSpec,
		Endpoint:   url,
		metadata:   metadata,
	}

	log.Infof("👽 Initializing prometheus client with URL: %s", url)
	cfg := api.Config{
		Address: url,
		RoundTripper: authTransport{
			Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: &tls.Config{InsecureSkipVerify: tlsVerify}},
			token:     token,
			username:  username,
			password:  password,
		},
	}
	c, err := api.NewClient(cfg)
	if err != nil {
		return &p, err
	}
	p.api = apiv1.NewAPI(c)
	// Verify Prometheus connection prior returning
	if err := p.verifyConnection(); err != nil {
		return &p, err
	}
	if configSpec.GlobalConfig.MetricsProfile != "" {
		if err := p.readProfile(configSpec.GlobalConfig.MetricsProfile); err != nil {
			log.Fatalf("Metrics-profile error: %v", err.Error())
		}
	}
	return &p, nil
}

func (p *Prometheus) verifyConnection() error {
	_, err := p.api.Runtimeinfo(context.TODO())
	if err != nil {
		return err
	}
	log.Debug("Prometheus endpoint verified")
	return nil
}

// ReadProfile reads and parses metric profile configuration
func (p *Prometheus) readProfile(metricsProfile string) error {
	f, err := util.ReadConfig(metricsProfile)
	if err != nil {
		return fmt.Errorf("error reading metrics profile %s: %s", metricsProfile, err)
	}
	yamlDec := yaml.NewDecoder(f)
	yamlDec.KnownFields(true)
	if err = yamlDec.Decode(&p.MetricProfile); err != nil {
		return fmt.Errorf("error decoding metrics profile %s: %s", metricsProfile, err)
	}
	return nil
}

// ScrapeJobsMetrics gets all prometheus metrics required and handles them
func (p *Prometheus) ScrapeJobsMetrics(indexer *indexers.Indexer) error {
	start := p.JobList[0].Start
	end := p.JobList[len(p.JobList)-1].End
	elapsed := int(end.Sub(start).Minutes())
	var err error
	var v model.Value
	var renderedQuery bytes.Buffer
	vars := util.EnvToMap()
	vars["elapsed"] = fmt.Sprintf("%dm", elapsed)
	log.Infof("🔍 Scraping prometheus metrics for benchmark from %s to %s", start.Format(time.RFC3339), end.Format(time.RFC3339))
	for _, md := range p.MetricProfile {
		var datapoints []interface{}
		t, _ := template.New("").Parse(md.Query)
		t.Execute(&renderedQuery, vars)
		query := renderedQuery.String()
		renderedQuery.Reset()
		if md.Instant {
			log.Debugf("Instant query: %s", query)
			if v, err = p.Query(query, end); err != nil {
				log.Warnf("Error found with query %s: %s", query, err)
				continue
			}
			if err := p.parseVector(md.MetricName, query, v, &datapoints); err != nil {
				log.Warnf("Error found parsing result from query %s: %s", query, err)
			}
		} else {
			log.Debugf("Range query: %s", query)
			v, err = p.QueryRange(query, start, end)
			if err != nil {
				log.Warnf("Error found with query %s: %s", query, err)
				continue
			}
			if err := p.parseMatrix(md.MetricName, query, v, &datapoints); err != nil {
				log.Warnf("Error found parsing result from query %s: %s", query, err)
				continue
			}
		}
		if p.ConfigSpec.GlobalConfig.IndexerConfig.Enabled {
			(*indexer).Index(datapoints, indexers.IndexingOpts{MetricName: md.MetricName})
		}
	}
	return nil
}

func (p *Prometheus) parseVector(metricName, query string, value model.Value, metrics *[]interface{}) error {
	var jobName string
	var jobConfig config.Job
	data, ok := value.(model.Vector)
	if !ok {
		return fmt.Errorf("unsupported result format: %s", value.Type().String())
	}
	for _, v := range data {
		for _, prometheusJob := range p.JobList {
			if v.Timestamp.Time().Before(prometheusJob.End) {
				jobName = prometheusJob.Name
				jobConfig = prometheusJob.JobConfig
				jobConfig.NamespaceLabels = nil // no need to insert this into the metric
				jobConfig.Objects = nil         // no need to insert this into the metric
			}
		}
		m := metric{
			Labels:     make(map[string]string),
			UUID:       p.UUID,
			Query:      query,
			MetricName: metricName,
			JobName:    jobName,
			JobConfig:  jobConfig,
			Metadata:   p.metadata,
		}
		for k, v := range v.Metric {
			if k == "__name__" {
				continue
			}
			m.Labels[string(k)] = string(v)
		}
		if math.IsNaN(float64(v.Value)) {
			m.Value = 0
		} else {
			m.Value = float64(v.Value)
		}
		m.Timestamp = v.Timestamp.Time()
		*metrics = append(*metrics, m)
	}
	return nil
}

func (p *Prometheus) parseMatrix(metricName, query string, value model.Value, metrics *[]interface{}) error {
	var jobName string
	var jobConfig config.Job
	data, ok := value.(model.Matrix)
	if !ok {
		return fmt.Errorf("unsupported result format: %s", value.Type().String())
	}
	for _, v := range data {
		for _, val := range v.Values {
			for _, job := range p.JobList {
				if val.Timestamp.Time().Before(job.End) {
					jobName = job.Name
					jobConfig = job.JobConfig
					jobConfig.NamespaceLabels = nil // no need to insert this into the metric
					jobConfig.Objects = nil         // no need to insert this into the metric.
				}
			}
			m := metric{
				Labels:     make(map[string]string),
				UUID:       p.UUID,
				Query:      query,
				MetricName: metricName,
				JobName:    jobName,
				JobConfig:  jobConfig,
				Timestamp:  val.Timestamp.Time(),
				Metadata:   p.metadata,
			}
			for k, v := range v.Metric {
				if k == "__name__" {
					continue
				}
				m.Labels[string(k)] = string(v)
			}
			if math.IsNaN(float64(val.Value)) {
				m.Value = 0
			} else {
				m.Value = float64(val.Value)
			}
			*metrics = append(*metrics, m)
		}
	}
	return nil
}

// Query prometheus query wrapper
func (p *Prometheus) Query(query string, time time.Time) (model.Value, error) {
	var v model.Value
	v, _, err := p.api.Query(context.TODO(), query, time)
	if err != nil {
		return v, err
	}
	return v, nil
}

// QueryRange prometheus queryRange wrapper
func (p *Prometheus) QueryRange(query string, start, end time.Time) (model.Value, error) {
	var v model.Value
	r := apiv1.Range{Start: start, End: end, Step: p.Step}
	v, _, err := p.api.QueryRange(context.TODO(), query, r)
	if err != nil {
		return v, err
	}
	return v, nil
}
