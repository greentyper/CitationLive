import unittest
from unittest.mock import call, patch
from urllib.parse import urlencode

import connect_google


def consent_url(redirect_uri):
    return 'https://accounts.google.com/o/oauth2/v2/auth?' + urlencode(
        {'redirect_uri': redirect_uri}
    )


class GoogleConsentTests(unittest.TestCase):
    @patch.object(connect_google, 'local_request')
    def test_desktop_sign_in_does_not_upload_a_client(self, local_request):
        url = consent_url(connect_google.BASE)
        local_request.side_effect = [
            {'configured': True}, {'ok': True, 'auth_url': url},
        ]
        self.assertEqual(connect_google.start_consent(), url)
        self.assertEqual(
            local_request.call_args_list,
            [call('/oauth/status'), call('/oauth/start')],
        )

    @patch.object(connect_google, 'load_client')
    @patch.object(connect_google, 'local_request')
    def test_custom_web_client_still_posts_configuration(self, local_request, load_client):
        url = consent_url(connect_google.CALLBACK)
        local_request.side_effect = [
            {'ok': True}, {'ok': True, 'auth_url': url},
        ]
        self.assertEqual(connect_google.start_consent('test-client.json'), url)
        load_client.assert_called_once_with('test-client.json')
        self.assertEqual(
            local_request.call_args_list,
            [call('/oauth/client', load_client.return_value), call('/oauth/start')],
        )

    @patch.object(connect_google, 'local_request')
    def test_unconfigured_build_does_not_start_consent(self, local_request):
        local_request.return_value = {'configured': False}
        with self.assertRaises(connect_google.ClientNotConfigured):
            connect_google.start_consent()
        local_request.assert_called_once_with('/oauth/status')

    def test_other_callback_is_rejected(self):
        with self.assertRaises(ValueError):
            connect_google.validate_consent_url(consent_url('http://127.0.0.1:99'))


if __name__ == '__main__':
    unittest.main()
