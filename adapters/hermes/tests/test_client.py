import os
import unittest
from unittest.mock import patch

from dots_lite_hermes.client import HubClient, HubError


class ClientEnvironmentTests(unittest.TestCase):
    def test_current_socket_environment_selects_its_endpoint(self):
        with patch.dict(os.environ, {
            "DOTS_LITE_SOCKET": "/private/current/hub.sock",
            "DOTS_DUET_SOCKET": "/private/old/hub.sock",
        }, clear=True):
            client = HubClient.from_environment()
        self.assertEqual(client.socket_path, "/private/current/hub.sock")

    def test_old_socket_environment_alone_does_not_configure_a_client(self):
        with patch.dict(os.environ, {"DOTS_DUET_SOCKET": "/private/old/hub.sock"}, clear=True):
            with self.assertRaises(HubError):
                HubClient.from_environment()
