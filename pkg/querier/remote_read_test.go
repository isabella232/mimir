// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/pkg/querier/remote_read_test.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Cortex Authors.

package querier

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-kit/log"
	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/prompb"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/storage"
	prom_remote "github.com/prometheus/prometheus/storage/remote"
	"github.com/stretchr/testify/require"

	"github.com/grafana/mimir/pkg/ingester/client"
	"github.com/grafana/mimir/pkg/mimirpb"
	"github.com/grafana/mimir/pkg/storage/series"
)

func TestSampledRemoteRead(t *testing.T) {
	suite, err := promql.NewTest(t, `
		load 1m
			test_metric1{foo="bar",baz="qux"} 1
	`)
	require.NoError(t, err)

	defer suite.Close()

	handler := RemoteReadHandler(suite.Storage(), log.NewNopLogger())

	requestBody, err := proto.Marshal(&client.ReadRequest{
		Queries: []*client.QueryRequest{
			{StartTimestampMs: 0, EndTimestampMs: 1},
		},
	})
	require.NoError(t, err)
	requestBody = snappy.Encode(nil, requestBody)
	request, err := http.NewRequest("GET", "/query", bytes.NewReader(requestBody))
	require.NoError(t, err)
	request.Header.Set("X-Prometheus-Remote-Read-Version", "0.1.0")

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	require.Equal(t, 200, recorder.Result().StatusCode)
	require.Equal(t, []string([]string{"application/x-protobuf"}), recorder.Result().Header["Content-Type"])
	responseBody, err := ioutil.ReadAll(recorder.Result().Body)
	require.NoError(t, err)
	responseBody, err = snappy.Decode(nil, responseBody)
	require.NoError(t, err)
	var response client.ReadResponse
	err = proto.Unmarshal(responseBody, &response)
	require.NoError(t, err)

	expected := client.ReadResponse{
		Results: []*client.QueryResponse{
			{
				Timeseries: []mimirpb.TimeSeries{
					{
						Labels: []mimirpb.LabelAdapter{
							{Name: "foo", Value: "bar"},
						},
						Samples: []mimirpb.Sample{
							{Value: 0, TimestampMs: 0},
							{Value: 1, TimestampMs: 1},
							{Value: 2, TimestampMs: 2},
							{Value: 3, TimestampMs: 3},
						},
					},
				},
			},
		},
	}
	require.Equal(t, expected, response)
}

func TestStreamedRemoteRead(t *testing.T) {
	// First with 120 samples. We expect 1 frame with 1 chunk.
	// Second with 121 samples, We expect 1 frame with 2 chunks.
	// Third with 241 samples. We expect 1 frame with 2 chunks, and 1 frame with 1 chunk for the same series due to bytes limit.
	suite, err := promql.NewTest(t, `
		load 1m
			test_metric1{foo="bar1",baz="qux",b="c",d="e"} 0+100x119
            test_metric1{foo="bar2",baz="qux",b="c",d="e"} 0+100x120
            test_metric1{foo="bar3",baz="qux",b="c",d="e"} 0+100x240
	`)
	require.NoError(t, err)

	defer suite.Close()

	require.NoError(t, suite.Run())

	handler := RemoteReadHandler(suite.Storage(), log.NewNopLogger())

	// Encode the request.
	matcher1, err := labels.NewMatcher(labels.MatchEqual, "__name__", "test_metric1")
	require.NoError(t, err)

	matcher2, err := labels.NewMatcher(labels.MatchEqual, "d", "e")
	require.NoError(t, err)

	matcher3, err := labels.NewMatcher(labels.MatchEqual, "foo", "bar1")
	require.NoError(t, err)

	query1, err := prom_remote.ToQuery(0, 14400001, []*labels.Matcher{matcher1, matcher2}, &storage.SelectHints{
		Start: 0,
		End:   14400001,
	})
	require.NoError(t, err)

	query2, err := prom_remote.ToQuery(0, 14400001, []*labels.Matcher{matcher1, matcher3}, &storage.SelectHints{
		Start: 0,
		End:   14400001,
	})
	require.NoError(t, err)

	req := &prompb.ReadRequest{
		Queries:               []*prompb.Query{query1, query2},
		AcceptedResponseTypes: []prompb.ReadRequest_ResponseType{prompb.ReadRequest_STREAMED_XOR_CHUNKS},
	}
	data, err := proto.Marshal(req)
	require.NoError(t, err)

	compressed := snappy.Encode(nil, data)
	request, err := http.NewRequest("POST", "", bytes.NewBuffer(compressed))
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	require.Equal(t, 2, recorder.Code/100)

	require.Equal(t, "application/x-streamed-protobuf; proto=prometheus.ChunkedReadResponse", recorder.Result().Header.Get("Content-Type"))
	require.Equal(t, "", recorder.Result().Header.Get("Content-Encoding"))

	var results []*prompb.ChunkedReadResponse
	stream := prom_remote.NewChunkedReader(recorder.Result().Body, prom_remote.DefaultChunkedReadLimit, nil)
	for {
		res := &prompb.ChunkedReadResponse{}
		err := stream.NextProto(res)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		results = append(results, res)
	}

	require.Equal(t, 5, len(results), "Expected 5 results.")

	require.Equal(t, []*prompb.ChunkedReadResponse{
		{
			ChunkedSeries: []*prompb.ChunkedSeries{
				{
					Labels: []prompb.Label{
						{Name: "__name__", Value: "test_metric1"},
						{Name: "b", Value: "c"},
						{Name: "baz", Value: "qux"},
						{Name: "d", Value: "e"},
						{Name: "foo", Value: "bar1"},
					},
					Chunks: []prompb.Chunk{
						{
							Type:      prompb.Chunk_XOR,
							MaxTimeMs: 7140000,
							Data:      []byte("\000x\000\000\000\000\000\000\000\000\000\340\324\003\302|\005\224\000\301\254}\351z2\320O\355\264n[\007\316\224\243md\371\320\375\032Pm\nS\235\016Q\255\006P\275\250\277\312\201Z\003(3\240R\207\332\005(\017\240\322\201\332=(\023\2402\203Z\007(w\2402\201Z\017(\023\265\227\364P\033@\245\007\364\nP\033C\245\002t\036P+@e\036\364\016Pk@e\002t:P;A\245\001\364\nS\373@\245\006t\006P+C\345\002\364\006Pk@\345\036t\nP\033A\245\003\364:P\033@\245\006t\016ZJ\377\\\205\313\210\327\270\017\345+F[\310\347E)\355\024\241\366\342}(v\215(N\203)\326\207(\336\203(V\332W\362\202t4\240m\005(\377AJ\006\320\322\202t\374\240\255\003(oA\312:\3202"),
						},
					},
				},
			},
		},
		{
			ChunkedSeries: []*prompb.ChunkedSeries{
				{
					Labels: []prompb.Label{
						{Name: "__name__", Value: "test_metric1"},
						{Name: "b", Value: "c"},
						{Name: "baz", Value: "qux"},
						{Name: "d", Value: "e"},
						{Name: "foo", Value: "bar2"},
					},
					Chunks: []prompb.Chunk{
						{
							Type:      prompb.Chunk_XOR,
							MaxTimeMs: 7140000,
							Data:      []byte("\000x\000\000\000\000\000\000\000\000\000\340\324\003\302|\005\224\000\301\254}\351z2\320O\355\264n[\007\316\224\243md\371\320\375\032Pm\nS\235\016Q\255\006P\275\250\277\312\201Z\003(3\240R\207\332\005(\017\240\322\201\332=(\023\2402\203Z\007(w\2402\201Z\017(\023\265\227\364P\033@\245\007\364\nP\033C\245\002t\036P+@e\036\364\016Pk@e\002t:P;A\245\001\364\nS\373@\245\006t\006P+C\345\002\364\006Pk@\345\036t\nP\033A\245\003\364:P\033@\245\006t\016ZJ\377\\\205\313\210\327\270\017\345+F[\310\347E)\355\024\241\366\342}(v\215(N\203)\326\207(\336\203(V\332W\362\202t4\240m\005(\377AJ\006\320\322\202t\374\240\255\003(oA\312:\3202"),
						},
						{
							Type:      prompb.Chunk_XOR,
							MinTimeMs: 7200000,
							MaxTimeMs: 7200000,
							Data:      []byte("\000\001\200\364\356\006@\307p\000\000\000\000\000\000"),
						},
					},
				},
			},
		},
		{
			ChunkedSeries: []*prompb.ChunkedSeries{
				{
					Labels: []prompb.Label{
						{Name: "__name__", Value: "test_metric1"},
						{Name: "b", Value: "c"},
						{Name: "baz", Value: "qux"},
						{Name: "d", Value: "e"},
						{Name: "foo", Value: "bar3"},
					},
					Chunks: []prompb.Chunk{
						{
							Type:      prompb.Chunk_XOR,
							MaxTimeMs: 7140000,
							Data:      []byte("\000x\000\000\000\000\000\000\000\000\000\340\324\003\302|\005\224\000\301\254}\351z2\320O\355\264n[\007\316\224\243md\371\320\375\032Pm\nS\235\016Q\255\006P\275\250\277\312\201Z\003(3\240R\207\332\005(\017\240\322\201\332=(\023\2402\203Z\007(w\2402\201Z\017(\023\265\227\364P\033@\245\007\364\nP\033C\245\002t\036P+@e\036\364\016Pk@e\002t:P;A\245\001\364\nS\373@\245\006t\006P+C\345\002\364\006Pk@\345\036t\nP\033A\245\003\364:P\033@\245\006t\016ZJ\377\\\205\313\210\327\270\017\345+F[\310\347E)\355\024\241\366\342}(v\215(N\203)\326\207(\336\203(V\332W\362\202t4\240m\005(\377AJ\006\320\322\202t\374\240\255\003(oA\312:\3202"),
						},
						{
							Type:      prompb.Chunk_XOR,
							MinTimeMs: 7200000,
							MaxTimeMs: 14340000,
							Data:      []byte("\000x\200\364\356\006@\307p\000\000\000\000\000\340\324\003\340>\224\355\260\277\322\200\372\005(=\240R\207:\003(\025\240\362\201z\003(\365\240r\203:\005(\r\241\322\201\372\r(\r\240R\237:\007(5\2402\201z\037(\025\2402\203:\005(\375\240R\200\372\r(\035\241\322\201:\003(5\240r\326g\364\271\213\227!\253q\037\312N\340GJ\033E)\375\024\241\266\362}(N\217(V\203)\336\207(\326\203(N\334W\322\203\2644\240}\005(\373AJ\031\3202\202\264\374\240\275\003(kA\3129\320R\201\2644\240\375\264\277\322\200\332\005(3\240r\207Z\003(\027\240\362\201Z\003(\363\240R\203\332\005(\017\241\322\201\332\r(\023\2402\237Z\007(7\2402\201Z\037(\023\240\322\200\332\005(\377\240R\200\332\r "),
						},
					},
				},
			},
		},
		{
			ChunkedSeries: []*prompb.ChunkedSeries{
				{
					Labels: []prompb.Label{
						{Name: "__name__", Value: "test_metric1"},
						{Name: "b", Value: "c"},
						{Name: "baz", Value: "qux"},
						{Name: "d", Value: "e"},
						{Name: "foo", Value: "bar3"},
					},
					Chunks: []prompb.Chunk{
						{
							Type:      prompb.Chunk_XOR,
							MinTimeMs: 14400000,
							MaxTimeMs: 14400000,
							Data:      []byte("\000\001\200\350\335\r@\327p\000\000\000\000\000\000"),
						},
					},
				},
			},
		},
		{
			ChunkedSeries: []*prompb.ChunkedSeries{
				{
					Labels: []prompb.Label{
						{Name: "__name__", Value: "test_metric1"},
						{Name: "b", Value: "c"},
						{Name: "baz", Value: "qux"},
						{Name: "d", Value: "e"},
						{Name: "foo", Value: "bar1"},
					},
					Chunks: []prompb.Chunk{
						{
							Type:      prompb.Chunk_XOR,
							MaxTimeMs: 7140000,
							Data:      []byte("\000x\000\000\000\000\000\000\000\000\000\340\324\003\302|\005\224\000\301\254}\351z2\320O\355\264n[\007\316\224\243md\371\320\375\032Pm\nS\235\016Q\255\006P\275\250\277\312\201Z\003(3\240R\207\332\005(\017\240\322\201\332=(\023\2402\203Z\007(w\2402\201Z\017(\023\265\227\364P\033@\245\007\364\nP\033C\245\002t\036P+@e\036\364\016Pk@e\002t:P;A\245\001\364\nS\373@\245\006t\006P+C\345\002\364\006Pk@\345\036t\nP\033A\245\003\364:P\033@\245\006t\016ZJ\377\\\205\313\210\327\270\017\345+F[\310\347E)\355\024\241\366\342}(v\215(N\203)\326\207(\336\203(V\332W\362\202t4\240m\005(\377AJ\006\320\322\202t\374\240\255\003(oA\312:\3202"),
						},
					},
				},
			},
			QueryIndex: 1,
		},
	}, results)
}

type mockQuerier struct {
	matrix model.Matrix
}

func (m mockQuerier) Select(_ bool, sp *storage.SelectHints, matchers ...*labels.Matcher) storage.SeriesSet {
	if sp == nil {
		panic(fmt.Errorf("select params must be set"))
	}
	return series.MatrixToSeriesSet(m.matrix)
}

func (m mockQuerier) LabelValues(name string, matchers ...*labels.Matcher) ([]string, storage.Warnings, error) {
	return nil, nil, nil
}

func (m mockQuerier) LabelNames(matchers ...*labels.Matcher) ([]string, storage.Warnings, error) {
	return nil, nil, nil
}

func (mockQuerier) Close() error {
	return nil
}
