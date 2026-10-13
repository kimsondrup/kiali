import { render } from '@testing-library/react';
import { CheckConfigComp } from '../DiscoveryTracingConfig';
import type { TracingCheck } from '../../../types/TracingInfo';

rstest.mock('utils/I18nUtils', () => ({
  t: (key: string) => key,
  useKialiTranslation: () => ({
    t: (key: string) => key
  })
}));

const renderDiagnose = (tracingDiagnose: TracingCheck): HTMLElement => {
  const { container } = render(
    <CheckConfigComp
      cluster="east"
      externalServices={[]}
      kiosk=""
      setTracingDiagnose={() => {}}
      tracingDiagnose={tracingDiagnose}
    />
  );
  return container;
};

const messageOf = (container: HTMLElement): Element => {
  const message = container.querySelector('[data-test="tracing-diagnose-message"]');
  expect(message).not.toBeNull();
  return message!;
};

const found: TracingCheck = {
  code: 200,
  logLine: [],
  message: 'Found [1] possible tracing configuration(s)',
  validConfig: [{ provider: 'jaeger', url: 'http://tracing:16685', useGRPC: true }]
};

const notFound: TracingCheck = {
  code: 404,
  logLine: [],
  message: 'No valid tracing configuration found',
  validConfig: []
};

describe('DiscoveryTracingConfig', () => {
  it('lists the configurations found under the message', () => {
    const container = renderDiagnose(found);
    expect(messageOf(container).textContent).toBe(found.message);
    expect(container.textContent).toContain('Possible configuration(s) found for external_services.tracing');
  });

  it('styles a failed discovery apart from a successful one and lists nothing', () => {
    const success = messageOf(renderDiagnose(found)).className;
    const container = renderDiagnose(notFound);
    expect(messageOf(container).textContent).toBe(notFound.message);
    expect(messageOf(container).className).not.toBe(success);
    expect(container.textContent).not.toContain('Possible configuration(s) found');
  });

  it('styles a payload without a code as a success', () => {
    const success = messageOf(renderDiagnose(found)).className;
    const legacy = messageOf(renderDiagnose({ ...found, code: undefined })).className;
    expect(legacy).toBe(success);
  });
});
