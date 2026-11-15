"""Connect the user's own Google OAuth web client through a local ADB forward."""
from pathlib import Path
import argparse
import json
import urllib.error
import urllib.parse
import urllib.request
import webbrowser


BASE = 'http://127.0.0.1:28766'
CALLBACK = BASE + '/oauth/callback'


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        # Never forward a POST containing client credentials elsewhere.
        raise ValueError('Redirect refused')


def parse_client(document):
    web = document.get('web') if isinstance(document, dict) else None
    if not isinstance(web, dict):
        raise ValueError('Use your own Google OAuth web-client JSON')
    redirects = web.get('redirect_uris')
    if not isinstance(redirects, list) or CALLBACK not in redirects:
        raise ValueError('Register the localhost callback before continuing')
    client = web.get('client_id')
    secret = web.get('client_secret')
    if not isinstance(client, str) or not client or not isinstance(secret, str) or not secret:
        raise ValueError('Missing OAuth client fields')
    return {'client_id': client, 'client_secret': secret}


def load_client(source):
    source = source.resolve()
    public = Path(__file__).resolve().parents[1]
    if (public.parent / 'BUILD_INFO.json').is_file():
        public = public.parent
    if source == public or public in source.parents:
        raise ValueError('Private client file must be outside repository')
    if not source.is_file() or source.stat().st_size > 65536:
        raise ValueError('Choose a downloaded OAuth JSON file under 64 KiB')
    return parse_client(json.loads(source.read_text(encoding='utf-8-sig')))


def validate_consent_url(value):
    if not isinstance(value, str):
        raise ValueError('Missing consent URL')
    parsed = urllib.parse.urlparse(value)
    if (parsed.scheme != 'https' or parsed.netloc != 'accounts.google.com'
            or parsed.path != '/o/oauth2/v2/auth'):
        raise ValueError('Unexpected consent URL')
    query = urllib.parse.parse_qs(parsed.query)
    if query.get('redirect_uri') != [CALLBACK]:
        raise ValueError('Unexpected OAuth callback')
    return value


def local_request(path, data=None):
    if path not in ('/oauth/client', '/oauth/start'):
        raise ValueError('Unknown local endpoint')
    body = json.dumps(data).encode('utf-8') if data is not None else None
    request = urllib.request.Request(BASE + path, data=body, headers={'Content-Type': 'application/json'})
    # No environment proxy, cookies or redirects for private localhost traffic.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    with opener.open(request, timeout=15) as response:
        payload = response.read(65537)
    if len(payload) > 65536:
        raise ValueError('Oversized local response')
    result = json.loads(payload)
    if not isinstance(result, dict) or result.get('ok') is not True:
        raise ValueError('Local service refused the request')
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--client-json', required=True, type=Path,
                        help='Private OAuth WEB client JSON outside repository')
    args = parser.parse_args()
    try:
        client = load_client(args.client_json)
        print('Sending your OAuth client only to the local ADB-forwarded service.')
        local_request('/oauth/client', client)
        result = local_request('/oauth/start')
        consent = validate_consent_url(result.get('auth_url'))
        try:
            opened = webbrowser.open(consent)
        except Exception:
            opened = False
        if not opened:
            print('Browser did not open. Open this one-time consent link on this computer:')
            print(consent)
    except (OSError, ValueError, urllib.error.URLError):
        # Never echo the JSON, raw server error, client ID, secret or consent state.
        parser.exit(1, 'Google setup failed. Check local forward, WEB client file and registered callback. No credential details displayed.\n')
    print('Finish consent in the browser. Connection is confirmed only by the callback success page, not by opening the browser.')


if __name__ == '__main__':
    main()
