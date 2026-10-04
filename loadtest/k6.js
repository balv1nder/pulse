import http from 'k6/http';
import { check } from 'k6';

const BASE = __ENV.BASE_URL || 'http://localhost:8000';
const RATE = Number(__ENV.RATE || 1000);
const common = { executor: 'constant-arrival-rate', rate: RATE, timeUnit: '1s',
                 preAllocatedVUs: 1000, maxVUs: 3000 };

export const options = {
  summaryTrendStats: ['avg', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
  scenarios: {
    warmup:  { ...common, duration: '15s' },
    measure: { ...common, duration: '60s', startTime: '15s' },
  },
  thresholds: {
    'http_req_duration{scenario:measure}': ['p(99)<100'],
    'http_req_failed{scenario:measure}': ['rate<0.01'],
    'dropped_iterations{scenario:measure}': ['count<1'],
    http_reqs: ['count>1000'],
  },
};

export default function () {
  const id = Math.floor(Math.random() * 1000);
  const res = http.get(`${BASE}/items/${id}`, {
    headers: { 'X-Client-ID': `client-${__VU % 50}` },
  });
  check(res, { 'status is 200': (r) => r.status === 200 });
}