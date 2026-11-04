# Deploy on AWS Elastic Beanstalk

Runs the public Docker image on one EC2 instance. No build step: Elastic
Beanstalk pulls `ghcr.io/prm01/recura:latest`.

1. Make the `recura` package on GitHub public (Package settings, Change visibility).
2. Zip `Dockerrun.aws.json` on its own: `zip recura-eb.zip Dockerrun.aws.json`.
3. In the Elastic Beanstalk console, create a **Web server environment**:
   - Platform: **Docker**, running on 64bit Amazon Linux 2023
   - Application code: **Upload your code**, choose `recura-eb.zip`
   - Preset: **Single instance**
   - Instance type: **t3.micro** (x86; the image is built for amd64, so not t4g)
   - Environment property: `RECURA_CONFIG` = `/examples/config.public.json`
4. Wait for health to turn green, then open the environment URL.

With no model key the public demo runs on the mock models. If you add a real
key, keep the public rate limit and set a spending cap with your provider.

To pick up a new image after a push to `main`, choose **Upload and deploy**
with the same zip, or **Restart app server(s)**.
