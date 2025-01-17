// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/pkg/querier/remote_read.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Cortex Authors.

package querier

import (
	"net/http"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/prometheus/storage"

	"github.com/grafana/mimir/pkg/ingester/client"
	"github.com/grafana/mimir/pkg/mimirpb"
	"github.com/grafana/mimir/pkg/util"
	util_log "github.com/grafana/mimir/pkg/util/log"
)

// Queries are a set of matchers with time ranges - should not get into megabytes
const maxRemoteReadQuerySize = 1024 * 1024

// RemoteReadHandler handles Prometheus remote read requests.
func RemoteReadHandler(q storage.Queryable, logger log.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		var req client.ReadRequest
		logger := util_log.WithContext(r.Context(), logger)
		if _, err := util.ParseProtoReader(ctx, r.Body, int(r.ContentLength), maxRemoteReadQuerySize, nil, &req, util.RawSnappy); err != nil {
			level.Error(logger).Log("msg", "failed to parse proto", "err", err.Error())
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Fetch samples for all queries in parallel.
		resp := client.ReadResponse{
			Results: make([]*client.QueryResponse, len(req.Queries)),
		}
		errors := make(chan error)
		for i, qr := range req.Queries {
			go func(i int, qr *client.QueryRequest) {
				from, to, matchers, err := client.FromQueryRequest(qr)
				if err != nil {
					errors <- err
					return
				}

				querier, err := q.Querier(ctx, int64(from), int64(to))
				if err != nil {
					errors <- err
					return
				}

				params := &storage.SelectHints{
					Start: int64(from),
					End:   int64(to),
				}
				seriesSet := querier.Select(false, params, matchers...)
				resp.Results[i], err = seriesSetToQueryResponse(seriesSet)
				errors <- err
			}(i, qr)
		}

		var lastErr error
		for range req.Queries {
			err := <-errors
			if err != nil {
				lastErr = err
			}
		}
		if lastErr != nil {
			http.Error(w, lastErr.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Add("Content-Type", "application/x-protobuf")
		if err := util.SerializeProtoResponse(w, &resp, util.RawSnappy); err != nil {
			level.Error(logger).Log("msg", "error sending remote read response", "err", err)
		}
	})
}

func seriesSetToQueryResponse(s storage.SeriesSet) (*client.QueryResponse, error) {
	result := &client.QueryResponse{}

	for s.Next() {
		series := s.At()
		samples := []mimirpb.Sample{}
		it := series.Iterator()
		for it.Next() {
			t, v := it.At()
			samples = append(samples, mimirpb.Sample{
				TimestampMs: t,
				Value:       v,
			})
		}
		if err := it.Err(); err != nil {
			return nil, err
		}
		result.Timeseries = append(result.Timeseries, mimirpb.TimeSeries{
			Labels:  mimirpb.FromLabelsToLabelAdapters(series.Labels()),
			Samples: samples,
		})
	}

	return result, s.Err()
}
