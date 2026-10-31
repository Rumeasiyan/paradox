import json, secrets, sys, time
from uptime_kuma_api import UptimeKumaApi, MonitorType
for i in range(60):
    try:
        api = UptimeKumaApi("http://127.0.0.1:3001", timeout=20)
        break
    except Exception:
        time.sleep(2)
pw = secrets.token_urlsafe(16)
if api.need_setup():
    api.setup("e2e-admin", pw)
api.login("e2e-admin", pw)
out = {}
for name, interval in (("paradox-heartbeat", 90), ("paradox-failures", 600)):
    r = api.add_monitor(type=MonitorType.PUSH, name=name, interval=interval)
    out[name] = {"id": r["monitorID"], "token": api.get_monitor(r["monitorID"])["pushToken"]}
out["password"] = pw
json.dump(out, open(sys.argv[1], "w"))
print("monitors", {k: v["id"] for k, v in out.items() if k != "password"})
api.disconnect()
