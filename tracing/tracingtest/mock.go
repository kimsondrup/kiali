package tracingtest

import (
	"context"
	"time"

	"github.com/stretchr/testify/mock"

	"github.com/kiali/kiali/models"
	"github.com/kiali/kiali/tracing/jaeger/model"
)

type TracingClientMock struct {
	mock.Mock
}

func (j *TracingClientMock) GetAppTraces(ctx context.Context, ns, app string, query models.TracingQuery) (traces *model.TracingResponse, err error) {
	args := j.Called(ctx, ns, app, query)
	return args.Get(0).(*model.TracingResponse), args.Error(1)
}

func (j *TracingClientMock) GetTraceDetail(ctx context.Context, traceId string) (trace *model.TracingSingleTrace, err error) {
	args := j.Called(ctx, traceId)
	// A nil first value is a backend that has no such trace; asserting it to the
	// pointer type would panic before the handler could turn it into a 404.
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*model.TracingSingleTrace), args.Error(1)
}

func (j *TracingClientMock) GetErrorTraces(ctx context.Context, ns string, app string, duration time.Duration) (errorTraces int, err error) {
	args := j.Called(ctx, ns, app, duration)
	return args.Get(0).(int), args.Error(1)
}

func (j *TracingClientMock) GetServiceStatus(ctx context.Context) (available bool, err error) {
	args := j.Called(ctx)
	return args.Get(0).(bool), args.Error(1)
}

func (j *TracingClientMock) GetServices(ctx context.Context) (list []string, err error) {
	args := j.Called(ctx)
	return args.Get(0).([]string), args.Error(1)
}
