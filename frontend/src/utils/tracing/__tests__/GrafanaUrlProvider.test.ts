import { GrafanaUrlProvider } from '../UrlProviders/Grafana';
import type { SpanData } from 'types/TracingInfo';

type ExploreFilter = {
  operator: string;
  scope: string;
  tag: string;
  value: string[];
  valueType: string;
};

type ExplorePanes = {
  a: {
    panelsState?: { trace: { spanId: string } };
    queries: Array<{
      filters: ExploreFilter[];
      query: string;
    }>;
  };
};

describe('GrafanaUrlProvider.AppSearchUrl', () => {
  beforeAll(() => {
    Object.defineProperty(window, 'crypto', {
      configurable: true,
      value: {
        getRandomValues: (arr: Uint8Array) => {
          for (let i = 0; i < arr.length; i++) {
            arr[i] = i;
          }
          return arr;
        }
      }
    });
  });

  const provider = new GrafanaUrlProvider('http://grafana:3000', {
    datasource_uid: 'tempo-uid',
    orgID: '1'
  });

  const parsePanes = (url: string): ExplorePanes => {
    const panesParam = new URL(url).searchParams.get('panes');
    expect(panesParam).not.toBeNull();
    return JSON.parse(decodeURIComponent(panesParam!));
  };

  it('keeps span tag keys and values in the correct order', () => {
    const url = provider.AppSearchUrl('productpage', { from: 1000, to: 2000 }, { 'http.method': 'GET', error: 'true' });
    const panes = parsePanes(url);
    const filters = panes.a.queries[0].filters;

    expect(filters[0]).toMatchObject({
      tag: 'service.name',
      value: ['productpage'],
      scope: 'resource'
    });

    const tagFilters = filters.slice(1).map(({ tag, value, operator, scope, valueType }) => ({
      tag,
      value,
      operator,
      scope,
      valueType
    }));

    expect(tagFilters).toEqual(
      expect.arrayContaining([
        {
          tag: 'http.method',
          operator: '=',
          scope: 'span',
          value: ['GET'],
          valueType: 'string'
        },
        {
          tag: 'error',
          operator: '=',
          scope: 'span',
          value: ['true'],
          valueType: 'string'
        }
      ])
    );
    expect(tagFilters).toHaveLength(2);
  });

  it('builds filters for the Errors only tag set', () => {
    const url = provider.AppSearchUrl('reviews', { from: 1000, to: 2000 }, { error: 'true' });
    const panes = parsePanes(url);
    const errorFilter = panes.a.queries[0].filters.find(f => f.tag === 'error');

    expect(errorFilter).toMatchObject({
      tag: 'error',
      operator: '=',
      scope: 'span',
      value: ['true'],
      valueType: 'string'
    });
  });
});

describe('GrafanaUrlProvider.SpanUrl', () => {
  const provider = new GrafanaUrlProvider('http://grafana:3000', {
    datasource_uid: 'tempo-uid',
    orgID: '1'
  });

  // Kiali reports a span ID as the hex text Tempo's own search API reports, so the link uses it as
  // it stands. It used to arrive base64 from the trace detail endpoint and was decoded here.
  it('names the span by the hex ID the backend reports', () => {
    const span = {
      spanID: 'f5b63fe67ee27431',
      traceID: '3fed459787ea5c51070a19c4e6cd3040'
    } as SpanData;

    const panesParam = new URL(provider.SpanUrl(span)).searchParams.get('panes');
    expect(panesParam).not.toBeNull();
    const panes = JSON.parse(decodeURIComponent(panesParam!)) as ExplorePanes;

    expect(panes.a.panelsState?.trace.spanId).toEqual('f5b63fe67ee27431');
    expect(panes.a.queries[0].query).toEqual('3fed459787ea5c51070a19c4e6cd3040');
  });
});
