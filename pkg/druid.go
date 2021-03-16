package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bitly/go-simplejson"
	"github.com/grafadruid/go-druid"
	druidquerybuilder "github.com/grafadruid/go-druid/builder"
	druidquery "github.com/grafadruid/go-druid/builder/query"
	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/datasource"
	"github.com/grafana/grafana-plugin-sdk-go/backend/instancemgmt"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/grafana/grafana-plugin-sdk-go/data"
)

type druidQuery struct {
	Builder  map[string]interface{} `json:"builder"`
	Settings map[string]interface{} `json:"settings"`
}

type druidResponse struct {
	Columns []struct {
		Name string
		Type string
	}
	Rows [][]interface{}
}

func newDataSourceInstance(settings backend.DataSourceInstanceSettings) (instancemgmt.Instance, error) {
	data, err := simplejson.NewJson(settings.JSONData)
	if err != nil {
		return &druidInstanceSettings{}, err
	}
	secureData := settings.DecryptedSecureJSONData

	var druidOpts []druid.ClientOption
	if retryMax := data.Get("connection.retryableRetryMax").MustInt(-1); retryMax != -1 {
		druidOpts = append(druidOpts, druid.WithRetryMax(retryMax))
	}
	if retryWaitMin := data.Get("connection.retryableRetryWaitMin").MustInt(-1); retryWaitMin != -1 {
		druidOpts = append(druidOpts, druid.WithRetryWaitMin(time.Duration(retryWaitMin)*time.Millisecond))
	}
	if retryWaitMax := data.Get("connection.retryableRetryWaitMax").MustInt(-1); retryWaitMax != -1 {
		druidOpts = append(druidOpts, druid.WithRetryWaitMax(time.Duration(retryWaitMax)*time.Millisecond))
	}
	if basicAuth := data.Get("connection.basicAuth").MustBool(); basicAuth {
		druidOpts = append(druidOpts, druid.WithBasicAuth(data.Get("connection.basicAuthUser").MustString(), secureData["connection.basicAuthPassword"]))
	}

	c, err := druid.NewClient(data.Get("connection.url").MustString(), druidOpts...)
	if err != nil {
		return &druidInstanceSettings{}, err
	}
	return &druidInstanceSettings{
		client:                 c,
		queryContextParameters: data.Get("query.contextParameters").MustArray(),
	}, nil
}

type druidInstanceSettings struct {
	client                 *druid.Client
	queryContextParameters []interface{}
}

func (s *druidInstanceSettings) Dispose() {
	s.client.Close()
}

func newDatasource() datasource.ServeOpts {
	ds := &druidDatasource{
		im: datasource.NewInstanceManager(newDataSourceInstance),
	}

	return datasource.ServeOpts{
		QueryDataHandler:    ds,
		CheckHealthHandler:  ds,
		CallResourceHandler: ds,
	}
}

type druidDatasource struct {
	im instancemgmt.InstanceManager
}

func (ds *druidDatasource) CallResource(ctx context.Context, req *backend.CallResourceRequest, sender backend.CallResourceResponseSender) error {
	var err error
	var body interface{}
	var code int
	body = "Unknown error"
	code = 500
	switch req.Path {
	case "query-variable":
		switch req.Method {
		case "POST":
			body, err = ds.QueryVariableData(ctx, req)
			if err == nil {
				code = 200
			}
		default:
			body = "Method not supported"
		}
	default:
		body = "Path not supported"
	}
	resp := &backend.CallResourceResponse{Status: code}
	resp.Body, err = json.Marshal(body)
	sender.Send(resp)
	return nil
}

type grafanaMetricFindValue struct {
	Value interface{} `json:"value"`
	Text  string      `json:"text"`
}

func (ds *druidDatasource) QueryVariableData(ctx context.Context, req *backend.CallResourceRequest) ([]grafanaMetricFindValue, error) {
	log.DefaultLogger.Info("QUERY VARIABLE", "_________________________REQ___________________________", string(req.Body))
	s, err := ds.settings(req.PluginContext)
	if err != nil {
		return []grafanaMetricFindValue{}, err
	}
	return ds.queryVariable(req.Body, s)
}

func (ds *druidDatasource) queryVariable(qry []byte, s *druidInstanceSettings) ([]grafanaMetricFindValue, error) {
	log.DefaultLogger.Info("DRUID EXECUTE QUERY VARIABLE", "_________________________GRAFANA QUERY___________________________", string(qry))
	//feature: probably implement a short (1s ? 500ms ? configurable in datasource ? beware memory: constrain size ?) life cache (druidInstanceSettings.cache ?) and early return then
	response := []grafanaMetricFindValue{}
	q, stg, err := ds.prepareQuery(qry, s)
	if err != nil {
		return response, err
	}
	log.DefaultLogger.Info("DRUID EXECUTE QUERY VARIABLE", "_________________________DRUID QUERY___________________________", q)
	r, err := ds.executeQuery(q, s, stg)
	if err != nil {
		return response, err
	}
	log.DefaultLogger.Info("DRUID EXECUTE QUERY VARIABLE", "_________________________DRUID RESPONSE___________________________", r)
	response, err = ds.prepareVariableResponse(r, stg)
	log.DefaultLogger.Info("DRUID EXECUTE QUERY VARIABLE", "_________________________GRAFANA RESPONSE___________________________", response)
	return response, err
}

func (ds *druidDatasource) prepareVariableResponse(resp *druidResponse, settings map[string]interface{}) ([]grafanaMetricFindValue, error) {
	// refactor: probably some method that returns a container (make([]whattypeever, 0)) and its related appender func based on column type)
	response := []grafanaMetricFindValue{}
	for ic, c := range resp.Columns {
		for _, r := range resp.Rows {
			switch c.Type {
			case "string":
				if r[ic] != nil {
					response = append(response, grafanaMetricFindValue{Value: r[ic].(string), Text: r[ic].(string)})
				}
			case "float":
				if r[ic] != nil {
					response = append(response, grafanaMetricFindValue{Value: r[ic].(float64), Text: fmt.Sprintf("%f", r[ic].(float64))})
				}
			case "int":
				if r[ic] != nil {
					i, err := strconv.Atoi(r[ic].(string))
					if err != nil {
						i = 0
					}
					response = append(response, grafanaMetricFindValue{Value: i, Text: r[ic].(string)})
				}
			case "bool":
				var b bool
				var err error
				b, ok := r[ic].(bool)
				if !ok {
					b, err = strconv.ParseBool(r[ic].(string))
					if err != nil {
						b = false
					}
				}
				var i int
				if b {
					i = 1
				} else {
					i = 0
				}
				response = append(response, grafanaMetricFindValue{Value: i, Text: strconv.FormatBool(b)})
			case "time":
				var t time.Time
				var err error
				if r[ic] == nil {
					r[ic] = 0.0
				}
				switch r[ic].(type) {
				case string:
					t, err = time.Parse("2006-01-02T15:04:05.000Z", r[ic].(string))
					if err != nil {
						t = time.Now()
					}
				case float64:
					sec, dec := math.Modf(r[ic].(float64) / 1000)
					t = time.Unix(int64(sec), int64(dec*(1e9)))
				}
				response = append(response, grafanaMetricFindValue{Value: t.Unix(), Text: t.Format(time.UnixDate)})
			}
		}
	}
	return response, nil
}

func (ds *druidDatasource) CheckHealth(ctx context.Context, req *backend.CheckHealthRequest) (*backend.CheckHealthResult, error) {
	result := &backend.CheckHealthResult{
		Status:  backend.HealthStatusError,
		Message: "Can't connect to Druid",
	}

	i, err := ds.im.Get(req.PluginContext)
	if err != nil {
		result.Message = "Can't get Druid instance"
		return result, nil
	}

	status, _, err := i.(*druidInstanceSettings).client.Common().Status()
	if err != nil {
		result.Message = "Can't fetch Druid status"
		return result, nil
	}

	result.Status = backend.HealthStatusOk
	result.Message = fmt.Sprintf("Succesfully connected to Druid %s", status.Version)
	return result, nil
}

func (ds *druidDatasource) QueryData(ctx context.Context, req *backend.QueryDataRequest) (*backend.QueryDataResponse, error) {
	response := backend.NewQueryDataResponse()

	s, err := ds.settings(req.PluginContext)
	if err != nil {
		return response, err
	}

	for _, q := range req.Queries {
		response.Responses[q.RefID] = ds.query(q, s)
	}

	return response, nil
}

func (ds *druidDatasource) settings(ctx backend.PluginContext) (*druidInstanceSettings, error) {
	s, err := ds.im.Get(ctx)
	if err != nil {
		return nil, err
	}
	return s.(*druidInstanceSettings), nil
}

func (ds *druidDatasource) query(qry backend.DataQuery, s *druidInstanceSettings) backend.DataResponse {
	log.DefaultLogger.Info("DRUID EXECUTE QUERY", "_________________________GRAFANA QUERY___________________________", qry)
	//feature: probably implement a short (1s ? 500ms ? configurable in datasource ? beware memory: constrain size ?) life cache (druidInstanceSettings.cache ?) and early return then
	response := backend.DataResponse{}
	q, stg, err := ds.prepareQuery(qry.JSON, s)
	if err != nil {
		response.Error = err
		return response
	}
	log.DefaultLogger.Info("DRUID EXECUTE QUERY", "_________________________DRUID QUERY___________________________", q)
	r, err := ds.executeQuery(q, s, stg)
	if err != nil {
		response.Error = err
		return response
	}
	log.DefaultLogger.Info("DRUID EXECUTE QUERY", "_________________________DRUID RESPONSE___________________________", r)
	response, err = ds.prepareResponse(r, stg)
	if err != nil {
		//note: error could be set from prepareResponse but this gives a chance to react to error here
		response.Error = err
	}
	log.DefaultLogger.Info("DRUID EXECUTE QUERY", "_________________________GRAFANA RESPONSE___________________________", response)
	return response
}

func (ds *druidDatasource) prepareQuery(qry []byte, s *druidInstanceSettings) (druidquerybuilder.Query, map[string]interface{}, error) {
	var q druidQuery
	err := json.Unmarshal(qry, &q)
	if err != nil {
		return nil, nil, err
	}

	if queryContextParameters, ok := q.Settings["contextParameters"]; ok {
		q.Builder["context"] = ds.mergeQueryContexts(
			ds.prepareQueryContext(s.queryContextParameters),
			ds.prepareQueryContext(queryContextParameters.([]interface{})))
	} else {
		q.Builder["context"] = ds.prepareQueryContext(s.queryContextParameters)
	}

	jsonQuery, err := json.Marshal(q.Builder)

	if err != nil {
		return nil, nil, err
	}
	log.DefaultLogger.Info("DRUID EXECUTE QUERY", "_________________________DRUID JSON QUERY___________________________", string(jsonQuery))

	query, err := s.client.Query().Load(jsonQuery)
	//feature: could ensure __time column is selected, time interval is set based on qry given timerange and consider max data points ?

	return query, q.Settings, err
}

func (ds *druidDatasource) prepareQueryContext(parameters []interface{}) map[string]interface{} {
	ctx := make(map[string]interface{})
	if parameters != nil {
		for _, parameter := range parameters {
			p := parameter.(map[string]interface{})
			ctx[p["name"].(string)] = p["value"]
		}
	}
	return ctx
}

func (ds *druidDatasource) mergeQueryContexts(contexts ...map[string]interface{}) map[string]interface{} {
	ctx := make(map[string]interface{})
	for _, c := range contexts {
		for k, v := range c {
			ctx[k] = v
		}
	}
	return ctx
}

func (ds *druidDatasource) executeQuery(q druidquerybuilder.Query, s *druidInstanceSettings, settings map[string]interface{}) (*druidResponse, error) {
	// refactor: probably need to extract per-query preprocessor and postprocessor into a per-query file. load those "plugins" (ak. QueryProcessor ?) into a register and then do something like plugins[q.Type()].preprocess(q) and plugins[q.Type()].postprocess(r)
	r := &druidResponse{}
	qtyp := q.Type()
	switch qtyp {
	case "sql":
		q.(*druidquery.SQL).SetResultFormat("array").SetHeader(true)
	case "scan":
		q.(*druidquery.Scan).SetResultFormat("compactedList")
	}
	var result json.RawMessage
	_, err := s.client.Query().Execute(q, &result)
	if err != nil {
		return r, err
	}
	var detectColumnType = func(c *struct {
		Name string
		Type string
	}, pos int, rr [][]interface{}) {
		t := map[string]int{"nil": 0}
		for i := 0; i < len(rr); i += int(math.Ceil(float64(len(rr)) / 5.0)) {
			r := rr[i]
			switch r[pos].(type) {
			case string:
				v := r[pos].(string)
				_, err := strconv.Atoi(v)
				if err != nil {
					_, err := strconv.ParseBool(v)
					if err != nil {
						_, err := time.Parse("2006-01-02T15:04:05.000Z", v)
						if err != nil {
							t["string"]++
							continue
						}
						t["time"]++
						continue
					}
					t["bool"]++
					continue
				}
				t["int"]++
				continue
			case float64:
				if c.Name == "__time" || strings.Contains(strings.ToLower(c.Name), "time_") {
					t["time"]++
					continue
				}
				t["float"]++
				continue
			case bool:
				t["bool"]++
				continue
			}
		}
		election := func(values map[string]int) string {
			type kv struct {
				Key   string
				Value int
			}
			var ss []kv
			for k, v := range values {
				ss = append(ss, kv{k, v})
			}
			sort.Slice(ss, func(i, j int) bool {
				return ss[i].Value > ss[j].Value
			})
			if len(ss) == 2 {
				return ss[0].Key
			}
			return "string"
		}
		c.Type = election(t)
	}
	switch qtyp {
	case "sql":
		var sqlr []interface{}
		err := json.Unmarshal(result, &sqlr)
		if err == nil && len(sqlr) > 1 {
			for _, row := range sqlr[1:] {
				r.Rows = append(r.Rows, row.([]interface{}))
			}
			for i, c := range sqlr[0].([]interface{}) {
				col := struct {
					Name string
					Type string
				}{Name: c.(string)}
				detectColumnType(&col, i, r.Rows)
				r.Columns = append(r.Columns, col)
			}
		}
	case "timeseries":
		var tsr []map[string]interface{}
		err := json.Unmarshal(result, &tsr)
		if err == nil && len(tsr) > 0 {
			var columns = []string{"timestamp"}
			for c := range tsr[0]["result"].(map[string]interface{}) {
				columns = append(columns, c)
			}
			for _, result := range tsr {
				var row []interface{}
				t := result["timestamp"]
				if t == nil {
					//grand total, lets keep it last
					t = r.Rows[len(r.Rows)-1][0]
				}
				row = append(row, t)
				colResults := result["result"].(map[string]interface{})
				for _, c := range columns[1:] {
					row = append(row, colResults[c])
				}
				r.Rows = append(r.Rows, row)
			}
			for i, c := range columns {
				col := struct {
					Name string
					Type string
				}{Name: c}
				detectColumnType(&col, i, r.Rows)
				r.Columns = append(r.Columns, col)
			}
		}
	case "topN":
		var tn []map[string]interface{}
		err := json.Unmarshal(result, &tn)
		if err == nil && len(tn) > 0 {
			var columns = []string{"timestamp"}
			for c := range tn[0]["result"].([]interface{})[0].(map[string]interface{}) {
				columns = append(columns, c)
			}
			for _, result := range tn {
				for _, record := range result["result"].([]interface{}) {
					var row []interface{}
					row = append(row, result["timestamp"])
					o := record.(map[string]interface{})
					for _, c := range columns[1:] {
						row = append(row, o[c])
					}
					r.Rows = append(r.Rows, row)
				}
			}
			for i, c := range columns {
				col := struct {
					Name string
					Type string
				}{Name: c}
				detectColumnType(&col, i, r.Rows)
				r.Columns = append(r.Columns, col)
			}
		}
	case "groupBy":
		var gb []map[string]interface{}
		err := json.Unmarshal(result, &gb)
		if err == nil && len(gb) > 0 {
			var columns = []string{"timestamp"}
			for c := range gb[0]["event"].(map[string]interface{}) {
				columns = append(columns, c)
			}
			for _, result := range gb {
				var row []interface{}
				row = append(row, result["timestamp"])
				colResults := result["event"].(map[string]interface{})
				for _, c := range columns[1:] {
					row = append(row, colResults[c])
				}
				r.Rows = append(r.Rows, row)
			}
			for i, c := range columns {
				col := struct {
					Name string
					Type string
				}{Name: c}
				detectColumnType(&col, i, r.Rows)
				r.Columns = append(r.Columns, col)
			}
		}
	case "scan":
		var scanr []map[string]interface{}
		err := json.Unmarshal(result, &scanr)
		if err == nil && len(scanr) > 0 {
			for _, e := range scanr[0]["events"].([]interface{}) {
				r.Rows = append(r.Rows, e.([]interface{}))
			}
			for i, c := range scanr[0]["columns"].([]interface{}) {
				col := struct {
					Name string
					Type string
				}{Name: c.(string)}
				detectColumnType(&col, i, r.Rows)
				r.Columns = append(r.Columns, col)
			}
		}
	case "search":
		var s []map[string]interface{}
		err := json.Unmarshal(result, &s)
		if err == nil && len(s) > 0 {
			var columns = []string{"timestamp"}
			for c := range s[0]["result"].([]interface{})[0].(map[string]interface{}) {
				columns = append(columns, c)
			}
			for _, result := range s {
				for _, record := range result["result"].([]interface{}) {
					var row []interface{}
					row = append(row, result["timestamp"])
					o := record.(map[string]interface{})
					for _, c := range columns[1:] {
						row = append(row, o[c])
					}
					r.Rows = append(r.Rows, row)
				}
			}
			for i, c := range columns {
				col := struct {
					Name string
					Type string
				}{Name: c}
				detectColumnType(&col, i, r.Rows)
				r.Columns = append(r.Columns, col)
			}
		}
	case "timeBoundary":
		var tb []map[string]interface{}
		err := json.Unmarshal(result, &tb)
		if err == nil && len(tb) > 0 {
			var columns = []string{"timestamp"}
			for c := range tb[0]["result"].(map[string]interface{}) {
				columns = append(columns, c)
			}
			for _, result := range tb {
				var row []interface{}
				row = append(row, result["timestamp"])
				colResults := result["result"].(map[string]interface{})
				for _, c := range columns[1:] {
					row = append(row, colResults[c])
				}
				r.Rows = append(r.Rows, row)
			}
			for i, c := range columns {
				col := struct {
					Name string
					Type string
				}{Name: c}
				detectColumnType(&col, i, r.Rows)
				r.Columns = append(r.Columns, col)
			}
		}
	case "dataSourceMetadata":
		var dsm []map[string]interface{}
		err := json.Unmarshal(result, &dsm)
		if err == nil && len(dsm) > 0 {
			var columns = []string{"timestamp"}
			for c := range dsm[0]["result"].(map[string]interface{}) {
				columns = append(columns, c)
			}
			for _, result := range dsm {
				var row []interface{}
				row = append(row, result["timestamp"])
				colResults := result["result"].(map[string]interface{})
				for _, c := range columns[1:] {
					row = append(row, colResults[c])
				}
				r.Rows = append(r.Rows, row)
			}
			for i, c := range columns {
				col := struct {
					Name string
					Type string
				}{Name: c}
				detectColumnType(&col, i, r.Rows)
				r.Columns = append(r.Columns, col)
			}
		}
	case "segmentMetadata":
		var sm []map[string]interface{}
		err := json.Unmarshal(result, &sm)
		if err == nil && len(sm) > 0 {
			var columns []string
			switch settings["view"].(string) {
			case "base":
				for k, v := range sm[0] {
					if k != "aggregators" && k != "columns" && k != "timestampSpec" {
						if k == "intervals" {
							for i := range v.([]interface{}) {
								pos := strconv.Itoa(i)
								columns = append(columns, "interval_start_"+pos)
								columns = append(columns, "interval_stop_"+pos)
							}
						} else {
							columns = append(columns, k)
						}
					}
				}
				for _, result := range sm {
					var row []interface{}
					for _, c := range columns {
						var col interface{}
						if strings.HasPrefix(c, "interval_") {
							parts := strings.Split(c, "_")
							pos := 0
							if parts[1] == "stop" {
								pos = 1
							}
							idx, err := strconv.Atoi(parts[2])
							if err != nil {
								return r, errors.New("interval parsing goes wrong")
							}
							ii := result["intervals"].([]interface{})[idx]
							col = strings.Split(ii.(string), "/")[pos]
						} else {
							col = result[c]
						}
						row = append(row, col)
					}
					r.Rows = append(r.Rows, row)
				}
			case "aggregators":
				for _, v := range sm[0]["aggregators"].(map[string]interface{}) {
					columns = append(columns, "aggregator")
					for k := range v.(map[string]interface{}) {
						columns = append(columns, k)
					}
					break
				}
				for _, result := range sm {
					for k, v := range result["aggregators"].(map[string]interface{}) {
						var row []interface{}
						for _, c := range columns {
							var col interface{}
							if c == "aggregator" {
								col = k
							} else {
								col = v.(map[string]interface{})[c]
							}
							row = append(row, col)
						}
						r.Rows = append(r.Rows, row)
					}
				}
			case "columns":
				for _, v := range sm[0]["columns"].(map[string]interface{}) {
					columns = append(columns, "column")
					for k := range v.(map[string]interface{}) {
						columns = append(columns, k)
					}
					break
				}
				for _, result := range sm {
					for k, v := range result["columns"].(map[string]interface{}) {
						var row []interface{}
						for _, c := range columns {
							var col interface{}
							if c == "column" {
								col = k
							} else {
								col = v.(map[string]interface{})[c]
							}
							row = append(row, col)
						}
						r.Rows = append(r.Rows, row)
					}
				}
			case "timestampspec":
				for k := range sm[0]["timestampSpec"].(map[string]interface{}) {
					columns = append(columns, k)
				}
				for _, result := range sm {
					var row []interface{}
					for _, c := range columns {
						col := result["timestampSpec"].(map[string]interface{})[c]
						row = append(row, col)
					}
					r.Rows = append(r.Rows, row)
				}
			}
			for i, c := range columns {
				col := struct {
					Name string
					Type string
				}{Name: c}
				detectColumnType(&col, i, r.Rows)
				r.Columns = append(r.Columns, col)
			}

		}
	default:
		return r, errors.New("unknown query type")
	}
	return r, err
}

func (ds *druidDatasource) prepareResponse(resp *druidResponse, settings map[string]interface{}) (backend.DataResponse, error) {
	// refactor: probably some method that returns a container (make([]whattypeever, 0)) and its related appender func based on column type)
	response := backend.DataResponse{}
	frame := data.NewFrame("response")
	hideEmptyColumns, _ := settings["hideEmptyColumns"].(bool)
	format, ok := settings["format"]
  if !ok {
    format = "long"
  } else {
    format = format.(string)
  }
  if format == "log" {
    for ic, c := range resp.Columns {
      var ff []string
      ff = make([]string, 0)
      if c.Type == "string" && c.Name == "message" {
        for _, r := range resp.Rows {
          if r[ic] == nil {
            r[ic] = ""
          }
          ff = append(ff, r[ic].(string))
        }
        frame.Fields = append(frame.Fields, data.NewField("____message", nil, ff))
      }
    }
  }
	for ic, c := range resp.Columns {
		var ff interface{}
		columnIsEmpty := true
		switch c.Type {
		case "string":
			ff = make([]string, 0)
		case "float":
			ff = make([]float64, 0)
		case "int":
			ff = make([]int64, 0)
		case "bool":
			ff = make([]bool, 0)
		case "nil":
			ff = make([]string, 0)
		case "time":
			ff = make([]time.Time, 0)
		}
		for _, r := range resp.Rows {
			if columnIsEmpty && r[ic] != nil && r[ic] != "" {
				columnIsEmpty = false
			}
			switch c.Type {
			case "string":
				if r[ic] == nil {
					r[ic] = ""
				}
				ff = append(ff.([]string), r[ic].(string))
			case "float":
				if r[ic] == nil {
					r[ic] = 0.0
				}
				ff = append(ff.([]float64), r[ic].(float64))
			case "int":
				if r[ic] == nil {
					r[ic] = "0"
				}
				i, err := strconv.Atoi(r[ic].(string))
				if err != nil {
					i = 0
				}
				ff = append(ff.([]int64), int64(i))
			case "bool":
				var b bool
				var err error
				b, ok := r[ic].(bool)
				if !ok {
					b, err = strconv.ParseBool(r[ic].(string))
					if err != nil {
						b = false
					}
				}
				ff = append(ff.([]bool), b)
			case "nil":
				ff = append(ff.([]string), "nil")
			case "time":
				if r[ic] == nil {
					r[ic] = 0.0
				}
				switch r[ic].(type) {
				case string:
					t, err := time.Parse("2006-01-02T15:04:05.000Z", r[ic].(string))
					if err != nil {
						t = time.Now()
					}
					ff = append(ff.([]time.Time), t)
				case float64:
					sec, dec := math.Modf(r[ic].(float64) / 1000)
					ff = append(ff.([]time.Time), time.Unix(int64(sec), int64(dec*(1e9))))
				}
			}
		}
		if hideEmptyColumns && columnIsEmpty {
			continue
		}
		frame.Fields = append(frame.Fields, data.NewField(c.Name, nil, ff))
	}
	if format == "wide" && len(frame.Fields) > 0 {
		f, err := data.LongToWide(frame, nil)
		if err == nil {
			frame = f
		}
	} else if format == "log" && len(frame.Fields) > 0 {
		frame.SetMeta(&data.FrameMeta{PreferredVisualization: data.VisTypeLogs})
	}
	response.Frames = append(response.Frames, frame)
	return response, nil
}
