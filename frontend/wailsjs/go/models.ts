export namespace main {

	export class BatchItemView {
	    id: string;
	    batchId: string;
	    sequence: number;
	    relativePath?: string;
	    displayName: string;
	    declaredSize: number;
	    transferred: number;
	    state: string;
	    sha256?: string;
	    error?: string;

	    static createFrom(source: any = {}) {
	        return new BatchItemView(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.batchId = source["batchId"];
	        this.sequence = source["sequence"];
	        this.relativePath = source["relativePath"];
	        this.displayName = source["displayName"];
	        this.declaredSize = source["declaredSize"];
	        this.transferred = source["transferred"];
	        this.state = source["state"];
	        this.sha256 = source["sha256"];
	        this.error = source["error"];
	    }
	}
	export class BatchView {
	    id: string;
	    direction: string;
	    kind: string;
	    rootName?: string;
	    items: BatchItemView[];
	    totalItemCount: number;
	    declaredTotalBytes: number;
	    transferredBytes: number;
	    state: string;
	    approvalState: string;
	    // Go type: time
	    createdAt: any;
	    // Go type: time
	    startedAt?: any;
	    // Go type: time
	    completedAt?: any;
	    error?: string;

	    static createFrom(source: any = {}) {
	        return new BatchView(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.direction = source["direction"];
	        this.kind = source["kind"];
	        this.rootName = source["rootName"];
	        this.items = this.convertValues(source["items"], BatchItemView);
	        this.totalItemCount = source["totalItemCount"];
	        this.declaredTotalBytes = source["declaredTotalBytes"];
	        this.transferredBytes = source["transferredBytes"];
	        this.state = source["state"];
	        this.approvalState = source["approvalState"];
	        this.createdAt = this.convertValues(source["createdAt"], null);
	        this.startedAt = this.convertValues(source["startedAt"], null);
	        this.completedAt = this.convertValues(source["completedAt"], null);
	        this.error = source["error"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class DownloadView {
	    id: string;
	    fileId: string;
	    name: string;
	    size: number;
	    // Go type: time
	    created: any;
	    state: string;
	    written: number;
	    speed: number;
	    reason?: string;

	    static createFrom(source: any = {}) {
	        return new DownloadView(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.fileId = source["fileId"];
	        this.name = source["name"];
	        this.size = source["size"];
	        this.created = this.convertValues(source["created"], null);
	        this.state = source["state"];
	        this.written = source["written"];
	        this.speed = source["speed"];
	        this.reason = source["reason"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class SessionView {
	    key: string;
	    // Go type: time
	    created: any;
	    // Go type: time
	    expiresAt: any;
	    pinnedTransfers: number;
	    token?: string;
	    interfaceName?: string;
	    address?: string;

	    static createFrom(source: any = {}) {
	        return new SessionView(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.key = source["key"];
	        this.created = this.convertValues(source["created"], null);
	        this.expiresAt = this.convertValues(source["expiresAt"], null);
	        this.pinnedTransfers = source["pinnedTransfers"];
	        this.token = source["token"];
	        this.interfaceName = source["interfaceName"];
	        this.address = source["address"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class SharedFileView {
	    id: string;
	    name: string;
	    size: number;
	    // Go type: time
	    created: any;

	    static createFrom(source: any = {}) {
	        return new SharedFileView(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.size = source["size"];
	        this.created = this.convertValues(source["created"], null);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class SharedSourceView {
	    id: string;
	    kind: string;
	    name: string;
	    rootName?: string;
	    itemCount: number;
	    totalBytes: number;
	    // Go type: time
	    created: any;

	    static createFrom(source: any = {}) {
	        return new SharedSourceView(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.kind = source["kind"];
	        this.name = source["name"];
	        this.rootName = source["rootName"];
	        this.itemCount = source["itemCount"];
	        this.totalBytes = source["totalBytes"];
	        this.created = this.convertValues(source["created"], null);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class UploadView {
	    id: string;
	    name: string;
	    size: number;
	    // Go type: time
	    created: any;
	    state: string;
	    uploaded: number;
	    speed: number;
	    reason?: string;

	    static createFrom(source: any = {}) {
	        return new UploadView(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.size = source["size"];
	        this.created = this.convertValues(source["created"], null);
	        this.state = source["state"];
	        this.uploaded = source["uploaded"];
	        this.speed = source["speed"];
	        this.reason = source["reason"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace netenv {

	export class View {
	    name: string;
	    ip: string;
	    cidr: string;
	    friendlyName: string;
	    type: string;
	    metadataAvailable: boolean;
	    connectionName: string;
	    friendlyNameSource: string;
	    active: boolean;

	    static createFrom(source: any = {}) {
	        return new View(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.ip = source["ip"];
	        this.cidr = source["cidr"];
	        this.friendlyName = source["friendlyName"];
	        this.type = source["type"];
	        this.metadataAvailable = source["metadataAvailable"];
	        this.connectionName = source["connectionName"];
	        this.friendlyNameSource = source["friendlyNameSource"];
	        this.active = source["active"];
	    }
	}

}

