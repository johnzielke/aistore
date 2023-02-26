#
# Copyright (c) 2018-2023, NVIDIA CORPORATION. All rights reserved.
#
import unittest

from aistore.sdk import Client
from tests.utils import random_string, destroy_bucket
from tests.integration import CLUSTER_ENDPOINT


class TestJobOps(unittest.TestCase):  # pylint: disable=unused-variable
    def setUp(self) -> None:
        self.bck_name = random_string()

        self.client = Client(CLUSTER_ENDPOINT)

    def tearDown(self) -> None:
        """
        Cleanup after each test, destroy the bucket if it exists
        """
        destroy_bucket(self.client, self.bck_name)

    def test_job_start(self):
        self.client.bucket(self.bck_name).create()
        job_id = self.client.job(job_kind="lru").start()
        self.client.job(job_id=job_id).wait()


if __name__ == "__main__":
    unittest.main()