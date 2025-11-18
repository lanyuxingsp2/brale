import requests
import os
for key in list(os.environ.keys()):
    if key.lower().endswith('proxy'):
        os.environ.pop(key, None)
print(requests.get('http://127.0.0.1:9991').status_code)
