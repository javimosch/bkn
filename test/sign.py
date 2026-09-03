import hmac, hashlib, sys, time
body = sys.argv[1]
t = sys.argv[2] if len(sys.argv) > 2 and sys.argv[2] else str(int(time.time()))
print('t=' + t + ',v1=' + hmac.new(b'whsec_dogfood', (t + '.' + body).encode(), hashlib.sha256).hexdigest())
