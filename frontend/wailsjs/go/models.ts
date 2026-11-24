export namespace main {
	
	export class PreviewResult {
	    originalB64: string;
	    extractedB64: string;
	    error: string;
	
	    static createFrom(source: any = {}) {
	        return new PreviewResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.originalB64 = source["originalB64"];
	        this.extractedB64 = source["extractedB64"];
	        this.error = source["error"];
	    }
	}

}

