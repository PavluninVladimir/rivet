// This program is owned by Rivet, never supplied by the project or user.
import http from 'k6/http';
import { sleep } from 'k6';
import exec from 'k6/execution';
import { Counter, Rate, Trend } from 'k6/metrics';
const cfg = JSON.parse(open('./stage.json'));
const errors = new Rate('request_errors');
const routeRequests = cfg.profile.routes.map((_,i)=>new Counter(`route_${i}_requests`));
const routeLatency = cfg.profile.routes.map((_,i)=>new Trend(`route_${i}_latency`,true));
const routeErrors = cfg.profile.routes.map((_,i)=>new Rate(`route_${i}_errors`));
const p=cfg.profile, phase=cfg.phase;
const scenario = p.unit==='rps'
 ? {executor:'constant-arrival-rate',rate:phase.load,timeUnit:'1s',duration:`${phase.duration_s}s`,preAllocatedVUs:p.max_vus,maxVUs:p.max_vus,gracefulStop:'5s'}
 : {executor:'constant-vus',vus:phase.load,duration:`${phase.duration_s}s`,gracefulStop:'5s'};
export const options = {
 scenarios:{load:scenario}, discardResponseBodies:true, maxRedirects:0,
 systemTags:['method','status','name','scenario','expected_response'],
 summaryTrendStats:['avg','min','max','p(95)','p(99)'],
 thresholds:{
  request_errors:[{threshold:`rate<=${p.max_error_rate_pct/100}`,abortOnFail:true,delayAbortEval:'5s'}],
  http_req_duration:[{threshold:`p(95)<=${p.p95_ms}`,abortOnFail:true,delayAbortEval:'5s'}],
  ...(p.unit==='rps'?{dropped_iterations:['count<=0']}:{}),
 },
};
const schedule=[];
for(let round=0;round<10;round++)p.routes.forEach((route,i)=>{if(route.weight>round)schedule.push(i)});
export default function(){
 // Absolute cutoff also survives loss of the supervising runner process.
 if(Date.now()+5000>=cfg.deadline_ms){exec.test.abort("immutable load deadline reached");return;}
 const i=schedule[exec.scenario.iterationInTest%schedule.length], route=p.routes[i];
 const headers={}; if(cfg.auth_header)headers[cfg.auth_header]=cfg.auth_value;
 const response=http.get(cfg.base+route.path,{headers,redirects:0,timeout:'5s',responseType:'none',tags:{name:`route_${i}`}});
 const contentType=(response.headers['Content-Type']||'').split(';')[0].trim().toLowerCase();
 const failed=response.status!==route.expected_status || !!route.expected_content_type&&contentType!==route.expected_content_type.toLowerCase();
 errors.add(failed);routeErrors[i].add(failed);routeRequests[i].add(1);routeLatency[i].add(response.timings.duration);
 if(p.unit==='vus')sleep(p.think_time_ms/1000);
}
export function handleSummary(data){return {'summary.json':JSON.stringify(data)}}
