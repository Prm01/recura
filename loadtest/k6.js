// k6 run loadtest/k6.js            (gateway at http://localhost:8080)
// k6 run -e BASE=https://recura.example.com -e KEY=sk-... loadtest/k6.js
import http from 'k6/http';
import { check } from 'k6';
import { Counter, Trend } from 'k6/metrics';

const BASE = __ENV.BASE || 'http://localhost:8080';
const KEY = __ENV.KEY || '';

const hits = new Counter('recura_hits');
const misses = new Counter('recura_misses');
const hitLatency = new Trend('recura_hit_latency', true);
const missLatency = new Trend('recura_miss_latency', true);

export const options = {
  scenarios: {
    ramp: {
      executor: 'ramping-arrival-rate',
      startRate: 10, timeUnit: '1s', preAllocatedVUs: 100, maxVUs: 500,
      stages: [{ target: 100, duration: '30s' }, { target: 300, duration: '1m' }, { target: 0, duration: '15s' }],
    },
  },
  thresholds: { http_req_failed: ['rate<0.01'], recura_hit_latency: ['p(95)<50'] },
};

const questions = [
  'how do i reset my password', 'i forgot my password', 'password bhool gaya kya karu', 'reset password please',
  'what is your refund policy', 'can i get my money back', 'refund kab milega',
  'how do i cancel my subscription', 'stop my subscription', 'how long does shipping take', 'when will my package arrive',
  'explain kafka simply', 'explain redis simply', 'what is graphql used for', 'pros and cons of rust',
];
const noise = ['', '', 'hey ', 'quick question ', 'pls ', 'urgent '];

export default function () {
  const q = noise[Math.floor(Math.random() * noise.length)] + questions[Math.floor(Math.random() * questions.length)];
  const headers = { 'Content-Type': 'application/json' };
  if (KEY) headers.Authorization = `Bearer ${KEY}`;
  const res = http.post(`${BASE}/v1/chat/completions`, JSON.stringify({ model: 'recura', messages: [{ role: 'user', content: q }] }), { headers });
  check(res, { 'status 200': (r) => r.status === 200 });
  if (res.headers['X-Recura-Cache'] === 'HIT') { hits.add(1); hitLatency.add(res.timings.duration); }
  else { misses.add(1); missLatency.add(res.timings.duration); }
}
