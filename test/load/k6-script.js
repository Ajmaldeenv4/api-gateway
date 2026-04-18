import http from 'k6/http';
import { check, sleep } from 'k6';

// Set TOKEN and GATEWAY_URL via env:
//   k6 run -e TOKEN=<jwt> -e GATEWAY=http://localhost:8080 k6-script.js
const gateway = __ENV.GATEWAY || 'http://localhost:8080';
const token   = __ENV.TOKEN  || '';

export const options = {
  stages: [
    { duration: '10s', target: 10 },
    { duration: '20s', target: 50 },
    { duration: '10s', target:  0 },
  ],
  thresholds: {
    http_req_duration: ['p(95)<500'],
    'http_req_failed{route:service-b}': ['rate<0.01'],
  },
};

export default function () {
  // Protected route — needs JWT.
  const resA = http.get(`${gateway}/a/ping`, {
    headers: { Authorization: `Bearer ${token}` },
    tags: { route: 'service-a' },
  });
  check(resA, {
    'a: 200 or 429': (r) => r.status === 200 || r.status === 429,
  });

  // Open route.
  const resB = http.get(`${gateway}/b/ping`, { tags: { route: 'service-b' } });
  check(resB, { 'b: 200': (r) => r.status === 200 });

  sleep(0.1);
}
