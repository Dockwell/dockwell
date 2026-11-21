# Brand marks

Each service's own mark, shown beside its name where the panel offers it: a connection's storage or mail provider, a registry, a DNS provider, a Git host, an alert channel's service or a runtime's language. `brands.tsx` draws them inline, so a page asks no other host for them. They identify the services and remain their owners' trademarks, with their original rights; they aren't Dockwell's, and showing one says nothing of an endorsement.

`brands.tsx` was generated from the files below, keeping each path and colour as published. The marks from Simple Icons are single-colour, in the brand's colour as Simple Icons records it. Take a mark again from its source to change it, and update its row.

| Brand | Used for | From | File | Licence or terms | SHA-256 of the source (first 16) |
| --- | --- | --- | --- | --- | --- |
| Cloudflare | Cloudflare R2, Cloudflare DNS | Simple Icons 16.36.0 (npm `simple-icons`) | `icons/cloudflare.svg` | CC0-1.0 (Simple Icons); Cloudflare's trademark guidelines, cloudflare.com/trademark | package `2069d8e682dda3aa` |
| Backblaze | Backblaze B2 | Simple Icons 16.36.0 | `icons/backblaze.svg` | CC0-1.0 (Simple Icons) | package `2069d8e682dda3aa` |
| Google Cloud | Google Cloud Storage | Simple Icons 16.36.0 | `icons/googlecloud.svg` | CC0-1.0 (Simple Icons); Google's brand resource center | package `2069d8e682dda3aa` |
| Resend | Resend | Simple Icons 16.36.0 | `icons/resend.svg` | CC0-1.0 (Simple Icons); resend.com/brand | package `2069d8e682dda3aa` |
| Mailgun | Mailgun | Simple Icons 16.36.0 | `icons/mailgun.svg` | CC0-1.0 (Simple Icons) | package `2069d8e682dda3aa` |
| Brevo | Brevo | Simple Icons 16.36.0 | `icons/brevo.svg` | CC0-1.0 (Simple Icons) | package `2069d8e682dda3aa` |
| GitHub | GitHub, GitHub Container Registry | Simple Icons 16.36.0 | `icons/github.svg` | CC0-1.0 (Simple Icons); GitHub's logo guidelines | package `2069d8e682dda3aa` |
| Docker | Docker Hub | Simple Icons 16.36.0 | `icons/docker.svg` | CC0-1.0 (Simple Icons); Docker's trademark guidelines | package `2069d8e682dda3aa` |
| GitLab | GitLab's registry | Simple Icons 16.36.0 | `icons/gitlab.svg` | CC0-1.0 (Simple Icons); GitLab's brand guidelines | package `2069d8e682dda3aa` |
| Discord | Alert channels | Simple Icons 16.36.0 | `icons/discord.svg` | CC0-1.0 (Simple Icons); discord.com/branding | package `2069d8e682dda3aa` |
| Telegram | Alert channels | Simple Icons 16.36.0 | `icons/telegram.svg` | CC0-1.0 (Simple Icons) | package `2069d8e682dda3aa` |
| ntfy | Alert channels | Simple Icons 16.36.0 | `icons/ntfy.svg` | CC0-1.0 (Simple Icons) | package `2069d8e682dda3aa` |
| Gmail | Alert email's mail services | Simple Icons 16.36.0 | `icons/gmail.svg` | CC0-1.0 (Simple Icons); Google's brand resource center | package `2069d8e682dda3aa` |
| PHP | PHP runtimes | Simple Icons 16.36.0, from php.net/download-logos.php | `icons/php.svg` | CC-BY-SA-4.0: the PHP logo by the PHP project, as Simple Icons records it; this mark, single-colour, is shared under the same licence | package `2069d8e682dda3aa` |
| Node.js | Node.js runtimes | Simple Icons 16.36.0 | `icons/nodedotjs.svg` | CC0-1.0 (Simple Icons); nodejs.org/en/about/branding | package `2069d8e682dda3aa` |
| Python | Python runtimes | Simple Icons 16.36.0 | `icons/python.svg` | CC0-1.0 (Simple Icons); python.org/community/logos | package `2069d8e682dda3aa` |
| Go | Go runtimes | Simple Icons 16.36.0 | `icons/go.svg` | CC0-1.0 (Simple Icons); go.dev's brand guidelines | package `2069d8e682dda3aa` |
| OpenJDK (Duke) | Java runtimes | Simple Icons 16.36.0, from OpenJDK's Duke repository | `icons/openjdk.svg` | BSD-3-Clause, as Simple Icons records it: Duke, released by Sun Microsystems, now Oracle; notice below | package `2069d8e682dda3aa` |
| Amazon S3 | Amazon S3 | AWS Architecture Icons, release 07/31/2026, from aws.amazon.com/architecture/icons | `Arch_Storage/32/Arch_Amazon-Simple-Storage-Service_32.svg` | AWS's terms for its architecture icons; not in Simple Icons | `13081868eb7eb97c` |
| Amazon SES | Amazon SES | AWS Architecture Icons, release 07/31/2026 | `Arch_Business-Applications/32/Arch_Amazon-Simple-Email-Service_32.svg` | AWS's terms for its architecture icons; not in Simple Icons | `f76d547442ece021` |
| Amazon ECR | Amazon ECR | AWS Architecture Icons, release 07/31/2026 | `Arch_Containers/32/Arch_Amazon-Elastic-Container-Registry_32.svg` | AWS's terms for its architecture icons; not in Simple Icons | `15993cd6558664e5` |
| Postmark | Postmark | postmarkapp.com, the logo its own site serves | `/images/logo-stamp-simple.svg` | Postmark's (ActiveCampaign's) trademark, shown to name the service; not in Simple Icons, and Postmark publishes no press kit | `be44c8d132657e19` |
| Fastmail | Alert email's mail services | fastmail.com, the mark its own site serves | `/favicon.svg` | Fastmail's trademark, shown to name the service; not in Simple Icons, and Fastmail publishes no press kit | `5843ac656fddc888` |

The Simple Icons package is `simple-icons-16.36.0.tgz` from registry.npmjs.org (SHA-256 `2069d8e682dda3aa05fa71de36c0ab17984e62d2bd34d7082c4a85e0691b4235`). The AWS file is the icon package zip linked from that page (SHA-256 `d2d166c453526471749d520e0db022c459abef759d2946cf2dd1d1c992dc6526`).

## Nominative use

The AWS marks (S3, SES and ECR), Postmark's and Fastmail's are shown as nominative use: each only beside the service's own name, to say which service a connection, a registry or a mail service is, never as Dockwell's or to suggest an endorsement. AWS grants its architecture icons for diagrams, whitepapers, presentations, data sheets and posters, and doesn't name product interfaces; Postmark and Fastmail publish no terms for their marks. If any of them asks, swap its entry in `brands.tsx` for an `icon` from the product's set; nothing else changes.

## Services with no mark here

These get an icon from the product's own set on the same tile (`BrandMark`'s `icon`), never letters:

- **Another S3-compatible store** and **another mail server**: no brand to show.
- **Slack**: Simple Icons no longer carries its mark, and Slack's logo files are behind its brand center under its own terms. It keeps the chat icon until they're taken from there.
- **Microsoft 365 and Outlook.com**: Microsoft's trademark guidelines don't allow its logos without its permission. They keep the mail icon.
- **SendGrid**: not in Simple Icons, and Twilio's brand page publishes no logo files. It keeps the mail icon.

## Duke's licence

The OpenJDK mark is Duke, which Sun Microsystems released under the BSD licence:

Copyright (c) Sun Microsystems, Inc., now Oracle and/or its affiliates. All rights reserved.

Redistribution and use in source and binary forms, with or without modification, are permitted provided that the following conditions are met:

1. Redistributions of source code must retain the above copyright notice, this list of conditions and the following disclaimer.
2. Redistributions in binary form must reproduce the above copyright notice, this list of conditions and the following disclaimer in the documentation and/or other materials provided with the distribution.
3. Neither the name of the copyright holder nor the names of its contributors may be used to endorse or promote products derived from this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
